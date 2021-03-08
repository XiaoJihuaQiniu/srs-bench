// The MIT License (MIT)
//
// Copyright (c) 2021 srs-bench(ossrs)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
package vnet

import (
	"context"
	"flag"
	"fmt"
	"github.com/pion/logging"
	"github.com/pion/transport/vnet"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type MockUDPEchoServer struct {
	realServerAddr        *net.UDPAddr
	realServerReady       context.Context
	realServerReadyCancel context.CancelFunc
}

func NewMockUDPEchoServer() *MockUDPEchoServer {
	v := &MockUDPEchoServer{}
	v.realServerReady, v.realServerReadyCancel = context.WithCancel(context.Background())
	return v
}

func (v *MockUDPEchoServer) doMockUDPServer(ctx context.Context) error {
	// Listen to a random port.
	laddr, err := net.ResolveUDPAddr("udp4", "127.0.0.1:0")
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return err
	}

	v.realServerAddr = conn.LocalAddr().(*net.UDPAddr)
	v.realServerReadyCancel()

	// When system quit, interrupt client.
	selfKill, selfKillCancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		selfKillCancel()
		conn.Close()
	}()

	// Note that if they has the same ID, the address should not changed.
	addrs := make(map[string]net.Addr)

	// Start an echo UDP server.
	buf := make([]byte, 1500)
	for ctx.Err() == nil {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if selfKill.Err() == context.Canceled {
				return nil
			}
			return err
		} else if n == 0 || addr == nil {
			return fmt.Errorf("n=%v, addr=%v", n, addr)
		} else if nn, err := conn.WriteTo(buf[:n], addr); err != nil {
			return err
		} else if nn != n {
			return fmt.Errorf("nn=%v, n=%v", nn, n)
		}

		// Check the address, shold not change, use content as ID.
		clientID := string(buf[:n])
		if oldAddr, ok := addrs[clientID]; ok && oldAddr.String() != addr.String() {
			return fmt.Errorf("address change %v to %v", oldAddr.String(), addr.String())
		}
		addrs[clientID] = addr
	}

	return nil
}

var testTimeout = flag.Int("timeout", 5000, "For each case, the timeout in ms")

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

// vnet client:
//		10.0.0.11:5787
// proxy to real server:
//		192.168.1.10:8000
func TestUDPProxyOne2One(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var r0, r1, r2 error
	defer func() {
		if r0 != nil || r1 != nil || r2 != nil {
			t.Errorf("fail for ctx=%v, r0=%v, r1=%v, r2=%v", ctx.Err(), r0, r1, r2)
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Timeout, fail
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(*testTimeout) * time.Millisecond):
			r2 = fmt.Errorf("timeout")
		}
	}()

	// For utest, we always proxy vnet packets to the random port we listen to.
	mockServer := NewMockUDPEchoServer()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := mockServer.doMockUDPServer(ctx); err != nil {
			r0 = err
		}
	}()

	// Create a vent and proxy.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		// When real server is ready, start the vnet test.
		select {
		case <-ctx.Done():
			return
		case <-mockServer.realServerReady.Done():
		}

		doVnetProxy := func() error {
			router, err := vnet.NewRouter(&vnet.RouterConfig{
				CIDR:          "0.0.0.0/0",
				LoggerFactory: logging.NewDefaultLoggerFactory(),
			})
			if err != nil {
				return err
			}

			clientNetwork := vnet.NewNet(&vnet.NetConfig{
				StaticIP: "10.0.0.11",
			})
			if err = router.AddNet(clientNetwork); err != nil {
				return err
			}

			if err := router.Start(); err != nil {
				return err
			}
			defer router.Stop()

			proxy, err := NewProxy(router)
			if err != nil {
				return err
			}
			defer proxy.Close()

			// For utest, mock the target real server.
			proxy.mockRealServerAddr = mockServer.realServerAddr

			// The real server address to proxy to.
			// Note that for utest, we will proxy to a local address.
			serverAddr, err := net.ResolveUDPAddr("udp4", "192.168.1.10:8000")
			if err != nil {
				return err
			}

			if err := proxy.Proxy(clientNetwork, serverAddr); err != nil {
				return err
			}

			// Now, all packets from client, will be proxy to real server, vice versa.
			client, err := clientNetwork.ListenPacket("udp4", "10.0.0.11:5787")
			if err != nil {
				return err
			}

			// When system quit, interrupt client.
			selfKill, selfKillCancel := context.WithCancel(context.Background())
			go func() {
				<-ctx.Done()
				selfKillCancel()
				client.Close()
			}()

			for i := 0; i < 10; i++ {
				if _, err := client.WriteTo([]byte("Hello"), serverAddr); err != nil {
					return err
				}

				buf := make([]byte, 1500)
				if n, addr, err := client.ReadFrom(buf); err != nil {
					if selfKill.Err() == context.Canceled {
						return nil
					}
					return err
				} else if n != 5 || addr == nil {
					return fmt.Errorf("n=%v, addr=%v", n, addr)
				} else if string(buf[:n]) != "Hello" {
					return fmt.Errorf("data %v", buf[:n])
				}

				// Wait for awhile for each UDP packet, to simulate real network.
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(30 * time.Millisecond):
				}
			}

			return err
		}

		if err := doVnetProxy(); err != nil {
			r1 = err
		}
	}()
}

// vnet client:
//		10.0.0.11:5787
//		10.0.0.11:5788
// proxy to real server:
//		192.168.1.10:8000
func TestUDPProxyTwo2One(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var r0, r1, r2, r3 error
	defer func() {
		if r0 != nil || r1 != nil || r2 != nil || r3 != nil {
			t.Errorf("fail for ctx=%v, r0=%v, r1=%v, r2=%v, r3=%v", ctx.Err(), r0, r1, r2, r3)
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Timeout, fail
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(*testTimeout) * time.Millisecond):
			r2 = fmt.Errorf("timeout")
		}
	}()

	// For utest, we always proxy vnet packets to the random port we listen to.
	mockServer := NewMockUDPEchoServer()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := mockServer.doMockUDPServer(ctx); err != nil {
			r0 = err
		}
	}()

	// Create a vent and proxy.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		// When real server is ready, start the vnet test.
		select {
		case <-ctx.Done():
			return
		case <-mockServer.realServerReady.Done():
		}

		doVnetProxy := func() error {
			router, err := vnet.NewRouter(&vnet.RouterConfig{
				CIDR:          "0.0.0.0/0",
				LoggerFactory: logging.NewDefaultLoggerFactory(),
			})
			if err != nil {
				return err
			}

			clientNetwork := vnet.NewNet(&vnet.NetConfig{
				StaticIP: "10.0.0.11",
			})
			if err = router.AddNet(clientNetwork); err != nil {
				return err
			}

			if err := router.Start(); err != nil {
				return err
			}
			defer router.Stop()

			proxy, err := NewProxy(router)
			if err != nil {
				return err
			}
			defer proxy.Close()

			// For utest, mock the target real server.
			proxy.mockRealServerAddr = mockServer.realServerAddr

			// The real server address to proxy to.
			// Note that for utest, we will proxy to a local address.
			serverAddr, err := net.ResolveUDPAddr("udp4", "192.168.1.10:8000")
			if err != nil {
				return err
			}

			if err := proxy.Proxy(clientNetwork, serverAddr); err != nil {
				return err
			}

			handClient := func(address, echoData string) error {
				// Now, all packets from client, will be proxy to real server, vice versa.
				client, err := clientNetwork.ListenPacket("udp4", address)
				if err != nil {
					return err
				}

				// When system quit, interrupt client.
				selfKill, selfKillCancel := context.WithCancel(context.Background())
				go func() {
					<-ctx.Done()
					selfKillCancel()
					client.Close()
				}()

				for i := 0; i < 10; i++ {
					if _, err := client.WriteTo([]byte(echoData), serverAddr); err != nil {
						return err
					}

					buf := make([]byte, 1500)
					if n, addr, err := client.ReadFrom(buf); err != nil {
						if selfKill.Err() == context.Canceled {
							return nil
						}
						return err
					} else if n != len(echoData) || addr == nil {
						return fmt.Errorf("n=%v, addr=%v", n, addr)
					} else if string(buf[:n]) != echoData {
						return fmt.Errorf("data %v", buf[:n])
					}

					// Wait for awhile for each UDP packet, to simulate real network.
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(30 * time.Millisecond):
					}
				}

				return nil
			}

			client0, client0Cancel := context.WithCancel(context.Background())
			go func() {
				defer client0Cancel()
				address := "10.0.0.11:5787"
				if err := handClient(address, "Hello"); err != nil {
					r3 = fmt.Errorf("client %v err %v", address, err)
				}
			}()

			client1, client1Cancel := context.WithCancel(context.Background())
			go func() {
				defer client1Cancel()
				address := "10.0.0.11:5788"
				if err := handClient(address, "World"); err != nil {
					r3 = fmt.Errorf("client %v err %v", address, err)
				}
			}()

			select {
			case <-ctx.Done():
			case <-client0.Done():
			case <-client1.Done():
			}

			return err
		}

		if err := doVnetProxy(); err != nil {
			r1 = err
		}
	}()
}

// vnet client:
//		10.0.0.11:5787
// proxy to real server:
//		192.168.1.10:8000
//
// vnet client:
//		10.0.0.11:5788
// proxy to real server:
//		192.168.1.10:8000
func TestUDPProxyProxyTwice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var r0, r1, r2, r3 error
	defer func() {
		if r0 != nil || r1 != nil || r2 != nil || r3 != nil {
			t.Errorf("fail for ctx=%v, r0=%v, r1=%v, r2=%v, r3=%v", ctx.Err(), r0, r1, r2, r3)
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Timeout, fail
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(*testTimeout) * time.Millisecond):
			r2 = fmt.Errorf("timeout")
		}
	}()

	// For utest, we always proxy vnet packets to the random port we listen to.
	mockServer := NewMockUDPEchoServer()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := mockServer.doMockUDPServer(ctx); err != nil {
			r0 = err
		}
	}()

	// Create a vent and proxy.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		// When real server is ready, start the vnet test.
		select {
		case <-ctx.Done():
			return
		case <-mockServer.realServerReady.Done():
		}

		doVnetProxy := func() error {
			router, err := vnet.NewRouter(&vnet.RouterConfig{
				CIDR:          "0.0.0.0/0",
				LoggerFactory: logging.NewDefaultLoggerFactory(),
			})
			if err != nil {
				return err
			}

			clientNetwork := vnet.NewNet(&vnet.NetConfig{
				StaticIP: "10.0.0.11",
			})
			if err = router.AddNet(clientNetwork); err != nil {
				return err
			}

			if err := router.Start(); err != nil {
				return err
			}
			defer router.Stop()

			proxy, err := NewProxy(router)
			if err != nil {
				return err
			}
			defer proxy.Close()

			// For utest, mock the target real server.
			proxy.mockRealServerAddr = mockServer.realServerAddr

			// The real server address to proxy to.
			// Note that for utest, we will proxy to a local address.
			serverAddr, err := net.ResolveUDPAddr("udp4", "192.168.1.10:8000")
			if err != nil {
				return err
			}

			handClient := func(address, echoData string) error {
				// We proxy multiple times, for example, in publisher and player, both call
				// the proxy when got answer.
				if err := proxy.Proxy(clientNetwork, serverAddr); err != nil {
					return err
				}

				// Now, all packets from client, will be proxy to real server, vice versa.
				client, err := clientNetwork.ListenPacket("udp4", address)
				if err != nil {
					return err
				}

				// When system quit, interrupt client.
				selfKill, selfKillCancel := context.WithCancel(context.Background())
				go func() {
					<-ctx.Done()
					selfKillCancel()
					client.Close()
				}()

				for i := 0; i < 10; i++ {
					if _, err := client.WriteTo([]byte(echoData), serverAddr); err != nil {
						return err
					}

					buf := make([]byte, 1500)
					if n, addr, err := client.ReadFrom(buf); err != nil {
						if selfKill.Err() == context.Canceled {
							return nil
						}
						return err
					} else if n != len(echoData) || addr == nil {
						return fmt.Errorf("n=%v, addr=%v", n, addr)
					} else if string(buf[:n]) != echoData {
						return fmt.Errorf("data %v", buf[:n])
					}

					// Wait for awhile for each UDP packet, to simulate real network.
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(30 * time.Millisecond):
					}
				}

				return nil
			}

			client0, client0Cancel := context.WithCancel(context.Background())
			go func() {
				defer client0Cancel()
				address := "10.0.0.11:5787"
				if err := handClient(address, "Hello"); err != nil {
					r3 = fmt.Errorf("client %v err %v", address, err)
				}
			}()

			client1, client1Cancel := context.WithCancel(context.Background())
			go func() {
				defer client1Cancel()

				// Slower than client0, 60ms.
				// To simulate the real player or publisher, might not start at the same time.
				select {
				case <-ctx.Done():
					return
				case <-time.After(150 * time.Millisecond):
				}

				address := "10.0.0.11:5788"
				if err := handClient(address, "World"); err != nil {
					r3 = fmt.Errorf("client %v err %v", address, err)
				}
			}()

			select {
			case <-ctx.Done():
			case <-client0.Done():
			case <-client1.Done():
			}

			return err
		}

		if err := doVnetProxy(); err != nil {
			r1 = err
		}
	}()
}
