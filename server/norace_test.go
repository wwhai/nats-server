// Copyright 2018-2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !race && !skip_no_race_tests
// +build !race,!skip_no_race_tests

package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"

	"github.com/klauspost/compress/s2"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"golang.org/x/time/rate"
)

// IMPORTANT: Tests in this file are not executed when running with the -race flag.
//            The test name should be prefixed with TestNoRace so we can run only
//            those tests: go test -run=TestNoRace ...

func TestNoRaceAvoidSlowConsumerBigMessages(t *testing.T) {
	opts := DefaultOptions() // Use defaults to make sure they avoid pending slow consumer.
	opts.NoSystemAccount = true
	s := RunServer(opts)
	defer s.Shutdown()

	nc1, err := nats.Connect(fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer nc1.Close()

	nc2, err := nats.Connect(fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer nc2.Close()

	data := make([]byte, 1024*1024) // 1MB payload
	rand.Read(data)

	expected := int32(500)
	received := int32(0)

	done := make(chan bool)

	// Create Subscription.
	nc1.Subscribe("slow.consumer", func(m *nats.Msg) {
		// Just eat it so that we are not measuring
		// code time, just delivery.
		atomic.AddInt32(&received, 1)
		if received >= expected {
			done <- true
		}
	})

	// Create Error handler
	nc1.SetErrorHandler(func(c *nats.Conn, s *nats.Subscription, err error) {
		t.Fatalf("Received an error on the subscription's connection: %v\n", err)
	})

	nc1.Flush()

	for i := 0; i < int(expected); i++ {
		nc2.Publish("slow.consumer", data)
	}
	nc2.Flush()

	select {
	case <-done:
		return
	case <-time.After(10 * time.Second):
		r := atomic.LoadInt32(&received)
		if s.NumSlowConsumers() > 0 {
			t.Fatalf("Did not receive all large messages due to slow consumer status: %d of %d", r, expected)
		}
		t.Fatalf("Failed to receive all large messages: %d of %d\n", r, expected)
	}
}

func TestNoRaceRoutedQueueAutoUnsubscribe(t *testing.T) {
	optsA, err := ProcessConfigFile("./configs/seed.conf")
	require_NoError(t, err)
	optsA.NoSigs, optsA.NoLog = true, true
	optsA.NoSystemAccount = true
	srvA := RunServer(optsA)
	defer srvA.Shutdown()

	srvARouteURL := fmt.Sprintf("nats://%s:%d", optsA.Cluster.Host, srvA.ClusterAddr().Port)
	optsB := nextServerOpts(optsA)
	optsB.Routes = RoutesFromStr(srvARouteURL)

	srvB := RunServer(optsB)
	defer srvB.Shutdown()

	// Wait for these 2 to connect to each other
	checkClusterFormed(t, srvA, srvB)

	// Have a client connection to each server
	ncA, err := nats.Connect(fmt.Sprintf("nats://%s:%d", optsA.Host, optsA.Port))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer ncA.Close()

	ncB, err := nats.Connect(fmt.Sprintf("nats://%s:%d", optsB.Host, optsB.Port))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer ncB.Close()

	rbar := int32(0)
	barCb := func(m *nats.Msg) {
		atomic.AddInt32(&rbar, 1)
	}
	rbaz := int32(0)
	bazCb := func(m *nats.Msg) {
		atomic.AddInt32(&rbaz, 1)
	}

	// Create 125 queue subs with auto-unsubscribe to each server for
	// group bar and group baz. So 250 total per queue group.
	cons := []*nats.Conn{ncA, ncB}
	for _, c := range cons {
		for i := 0; i < 100; i++ {
			qsub, err := c.QueueSubscribe("foo", "bar", barCb)
			if err != nil {
				t.Fatalf("Error on subscribe: %v", err)
			}
			if err := qsub.AutoUnsubscribe(1); err != nil {
				t.Fatalf("Error on auto-unsubscribe: %v", err)
			}
			qsub, err = c.QueueSubscribe("foo", "baz", bazCb)
			if err != nil {
				t.Fatalf("Error on subscribe: %v", err)
			}
			if err := qsub.AutoUnsubscribe(1); err != nil {
				t.Fatalf("Error on auto-unsubscribe: %v", err)
			}
		}
		c.Subscribe("TEST.COMPLETE", func(m *nats.Msg) {})
	}

	// We coelasce now so for each server we will have all local (200) plus
	// two from the remote side for each queue group. We also create one more
	// and will wait til each server has 204 subscriptions, that will make sure
	// that we have everything setup.
	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		subsA := srvA.NumSubscriptions()
		subsB := srvB.NumSubscriptions()
		if subsA != 204 || subsB != 204 {
			return fmt.Errorf("Not all subs processed yet: %d and %d", subsA, subsB)
		}
		return nil
	})

	expected := int32(200)
	// Now send messages from each server
	for i := int32(0); i < expected; i++ {
		c := cons[i%2]
		c.Publish("foo", []byte("Don't Drop Me!"))
	}
	for _, c := range cons {
		c.Flush()
	}

	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		nbar := atomic.LoadInt32(&rbar)
		nbaz := atomic.LoadInt32(&rbaz)
		if nbar == expected && nbaz == expected {
			return nil
		}
		return fmt.Errorf("Did not receive all %d queue messages, received %d for 'bar' and %d for 'baz'",
			expected, atomic.LoadInt32(&rbar), atomic.LoadInt32(&rbaz))
	})
}

func TestNoRaceClosedSlowConsumerWriteDeadline(t *testing.T) {
	opts := DefaultOptions()
	opts.NoSystemAccount = true
	opts.WriteDeadline = 10 * time.Millisecond // Make very small to trip.
	opts.MaxPending = 500 * 1024 * 1024        // Set high so it will not trip here.
	s := RunServer(opts)
	defer s.Shutdown()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port), 3*time.Second)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("CONNECT {}\r\nPING\r\nSUB foo 1\r\n")); err != nil {
		t.Fatalf("Error sending protocols to server: %v", err)
	}
	// Reduce socket buffer to increase reliability of data backing up in the server destined
	// for our subscribed client.
	c.(*net.TCPConn).SetReadBuffer(128)

	url := fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port)
	sender, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 1024*1024)
	for i := 0; i < 100; i++ {
		if err := sender.Publish("foo", payload); err != nil {
			t.Fatalf("Error on publish: %v", err)
		}
	}

	// Flush sender connection to ensure that all data has been sent.
	if err := sender.Flush(); err != nil {
		t.Fatalf("Error on flush: %v", err)
	}

	// At this point server should have closed connection c.
	checkClosedConns(t, s, 1, 2*time.Second)
	conns := s.closedClients()
	if lc := len(conns); lc != 1 {
		t.Fatalf("len(conns) expected to be %d, got %d\n", 1, lc)
	}
	checkReason(t, conns[0].Reason, SlowConsumerWriteDeadline)
}

func TestNoRaceClosedSlowConsumerPendingBytes(t *testing.T) {
	opts := DefaultOptions()
	opts.NoSystemAccount = true
	opts.WriteDeadline = 30 * time.Second // Wait for long time so write deadline does not trigger slow consumer.
	opts.MaxPending = 1 * 1024 * 1024     // Set to low value (1MB) to allow SC to trip.
	s := RunServer(opts)
	defer s.Shutdown()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port), 3*time.Second)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("CONNECT {}\r\nPING\r\nSUB foo 1\r\n")); err != nil {
		t.Fatalf("Error sending protocols to server: %v", err)
	}
	// Reduce socket buffer to increase reliability of data backing up in the server destined
	// for our subscribed client.
	c.(*net.TCPConn).SetReadBuffer(128)

	url := fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port)
	sender, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 1024*1024)
	for i := 0; i < 100; i++ {
		if err := sender.Publish("foo", payload); err != nil {
			t.Fatalf("Error on publish: %v", err)
		}
	}

	// Flush sender connection to ensure that all data has been sent.
	if err := sender.Flush(); err != nil {
		t.Fatalf("Error on flush: %v", err)
	}

	// At this point server should have closed connection c.
	checkClosedConns(t, s, 1, 2*time.Second)
	conns := s.closedClients()
	if lc := len(conns); lc != 1 {
		t.Fatalf("len(conns) expected to be %d, got %d\n", 1, lc)
	}
	checkReason(t, conns[0].Reason, SlowConsumerPendingBytes)
}

func TestNoRaceSlowConsumerPendingBytes(t *testing.T) {
	opts := DefaultOptions()
	opts.NoSystemAccount = true
	opts.WriteDeadline = 30 * time.Second // Wait for long time so write deadline does not trigger slow consumer.
	opts.MaxPending = 1 * 1024 * 1024     // Set to low value (1MB) to allow SC to trip.
	s := RunServer(opts)
	defer s.Shutdown()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port), 3*time.Second)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("CONNECT {}\r\nPING\r\nSUB foo 1\r\n")); err != nil {
		t.Fatalf("Error sending protocols to server: %v", err)
	}
	// Reduce socket buffer to increase reliability of data backing up in the server destined
	// for our subscribed client.
	c.(*net.TCPConn).SetReadBuffer(128)

	url := fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port)
	sender, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 1024*1024)
	for i := 0; i < 100; i++ {
		if err := sender.Publish("foo", payload); err != nil {
			t.Fatalf("Error on publish: %v", err)
		}
	}

	// Flush sender connection to ensure that all data has been sent.
	if err := sender.Flush(); err != nil {
		t.Fatalf("Error on flush: %v", err)
	}

	// At this point server should have closed connection c.

	// On certain platforms, it may take more than one call before
	// getting the error.
	for i := 0; i < 100; i++ {
		if _, err := c.Write([]byte("PUB bar 5\r\nhello\r\n")); err != nil {
			// ok
			return
		}
	}
	t.Fatal("Connection should have been closed")
}

func TestNoRaceGatewayNoMissingReplies(t *testing.T) {
	// This test will have following setup:
	//
	// responder1		         requestor
	//    |                          |
	//    v                          v
	//   [A1]<-------gw------------[B1]
	//    |  \                      |
	//    |   \______gw__________   | route
	//    |                     _\| |
	//   [  ]--------gw----------->[  ]
	//   [A2]<-------gw------------[B2]
	//   [  ]                      [  ]
	//    ^
	//    |
	// responder2
	//
	// There is a possible race that when the requestor creates
	// a subscription on the reply subject, the subject interest
	// being sent from the inbound gateway, and B1 having none,
	// the SUB first goes to B2 before being sent to A1 from
	// B2's inbound GW. But the request can go from B1 to A1
	// right away and the responder1 connecting to A1 may send
	// back the reply before the interest on the reply makes it
	// to A1 (from B2).
	// This test will also verify that if the responder is instead
	// connected to A2, the reply is properly received by requestor
	// on B1.

	// For this test we want to be in interestOnly mode, so
	// make it happen quickly
	gatewayMaxRUnsubBeforeSwitch = 1
	defer func() { gatewayMaxRUnsubBeforeSwitch = defaultGatewayMaxRUnsubBeforeSwitch }()

	// Start with setting up A2 and B2.
	ob2 := testDefaultOptionsForGateway("B")
	sb2 := runGatewayServer(ob2)
	defer sb2.Shutdown()

	oa2 := testGatewayOptionsFromToWithServers(t, "A", "B", sb2)
	sa2 := runGatewayServer(oa2)
	defer sa2.Shutdown()

	waitForOutboundGateways(t, sa2, 1, time.Second)
	waitForInboundGateways(t, sa2, 1, time.Second)
	waitForOutboundGateways(t, sb2, 1, time.Second)
	waitForInboundGateways(t, sb2, 1, time.Second)

	// Now start A1 which will connect to B2
	oa1 := testGatewayOptionsFromToWithServers(t, "A", "B", sb2)
	oa1.Routes = RoutesFromStr(fmt.Sprintf("nats://%s:%d", oa2.Cluster.Host, oa2.Cluster.Port))
	sa1 := runGatewayServer(oa1)
	defer sa1.Shutdown()

	waitForOutboundGateways(t, sa1, 1, time.Second)
	waitForInboundGateways(t, sb2, 2, time.Second)

	checkClusterFormed(t, sa1, sa2)

	// Finally, start B1 that will connect to A1.
	ob1 := testGatewayOptionsFromToWithServers(t, "B", "A", sa1)
	ob1.Routes = RoutesFromStr(fmt.Sprintf("nats://%s:%d", ob2.Cluster.Host, ob2.Cluster.Port))
	sb1 := runGatewayServer(ob1)
	defer sb1.Shutdown()

	// Check that we have the outbound gateway from B1 to A1
	checkFor(t, 3*time.Second, 15*time.Millisecond, func() error {
		c := sb1.getOutboundGatewayConnection("A")
		if c == nil {
			return fmt.Errorf("Outbound connection to A not created yet")
		}
		c.mu.Lock()
		name := c.opts.Name
		nc := c.nc
		c.mu.Unlock()
		if name != sa1.ID() {
			// Force a disconnect
			nc.Close()
			return fmt.Errorf("Was unable to have B1 connect to A1")
		}
		return nil
	})

	waitForInboundGateways(t, sa1, 1, time.Second)
	checkClusterFormed(t, sb1, sb2)

	a1URL := fmt.Sprintf("nats://%s:%d", oa1.Host, oa1.Port)
	a2URL := fmt.Sprintf("nats://%s:%d", oa2.Host, oa2.Port)
	b1URL := fmt.Sprintf("nats://%s:%d", ob1.Host, ob1.Port)
	b2URL := fmt.Sprintf("nats://%s:%d", ob2.Host, ob2.Port)

	ncb1 := natsConnect(t, b1URL)
	defer ncb1.Close()

	ncb2 := natsConnect(t, b2URL)
	defer ncb2.Close()

	natsSubSync(t, ncb1, "just.a.sub")
	natsSubSync(t, ncb2, "just.a.sub")
	checkExpectedSubs(t, 2, sb1, sb2)

	// For this test, we want A to be checking B's interest in order
	// to send messages (which would cause replies to be dropped if
	// there is no interest registered on A). So from A servers,
	// send to various subjects and cause B's to switch to interestOnly
	// mode.
	nca1 := natsConnect(t, a1URL)
	defer nca1.Close()
	for i := 0; i < 10; i++ {
		natsPub(t, nca1, fmt.Sprintf("reject.%d", i), []byte("hello"))
	}
	nca2 := natsConnect(t, a2URL)
	defer nca2.Close()
	for i := 0; i < 10; i++ {
		natsPub(t, nca2, fmt.Sprintf("reject.%d", i), []byte("hello"))
	}

	checkSwitchedMode := func(t *testing.T, s *Server) {
		t.Helper()
		checkFor(t, 2*time.Second, 15*time.Millisecond, func() error {
			var switchedMode bool
			c := s.getOutboundGatewayConnection("B")
			ei, _ := c.gw.outsim.Load(globalAccountName)
			if ei != nil {
				e := ei.(*outsie)
				e.RLock()
				switchedMode = e.ni == nil && e.mode == InterestOnly
				e.RUnlock()
			}
			if !switchedMode {
				return fmt.Errorf("Still not switched mode")
			}
			return nil
		})
	}
	checkSwitchedMode(t, sa1)
	checkSwitchedMode(t, sa2)

	// Setup a subscriber on _INBOX.> on each of A's servers.
	total := 1000
	expected := int32(total)
	rcvOnA := int32(0)
	qrcvOnA := int32(0)
	natsSub(t, nca1, "myreply.>", func(_ *nats.Msg) {
		atomic.AddInt32(&rcvOnA, 1)
	})
	natsQueueSub(t, nca2, "myreply.>", "bar", func(_ *nats.Msg) {
		atomic.AddInt32(&qrcvOnA, 1)
	})
	checkExpectedSubs(t, 2, sa1, sa2)

	// Ok.. so now we will run the actual test where we
	// create a responder on A1 and make sure that every
	// single request from B1 gets the reply. Will repeat
	// test with responder connected to A2.
	sendReqs := func(t *testing.T, subConn *nats.Conn) {
		t.Helper()
		responder := natsSub(t, subConn, "foo", func(m *nats.Msg) {
			m.Respond([]byte("reply"))
		})
		natsFlush(t, subConn)
		checkExpectedSubs(t, 3, sa1, sa2)

		// We are not going to use Request() because this sets
		// a wildcard subscription on an INBOX and less likely
		// to produce the race. Instead we will explicitly set
		// the subscription on the reply subject and create one
		// per request.
		for i := 0; i < total/2; i++ {
			reply := fmt.Sprintf("myreply.%d", i)
			replySub := natsQueueSubSync(t, ncb1, reply, "bar")
			natsFlush(t, ncb1)

			// Let's make sure we have interest on B2.
			if r := sb2.globalAccount().sl.Match(reply); len(r.qsubs) == 0 {
				checkFor(t, time.Second, time.Millisecond, func() error {
					if r := sb2.globalAccount().sl.Match(reply); len(r.qsubs) == 0 {
						return fmt.Errorf("B still not registered interest on %s", reply)
					}
					return nil
				})
			}
			natsPubReq(t, ncb1, "foo", reply, []byte("request"))
			if _, err := replySub.NextMsg(time.Second); err != nil {
				t.Fatalf("Did not receive reply: %v", err)
			}
			natsUnsub(t, replySub)
		}

		responder.Unsubscribe()
		natsFlush(t, subConn)
		checkExpectedSubs(t, 2, sa1, sa2)
	}
	sendReqs(t, nca1)
	sendReqs(t, nca2)

	checkFor(t, time.Second, 15*time.Millisecond, func() error {
		if n := atomic.LoadInt32(&rcvOnA); n != expected {
			return fmt.Errorf("Subs on A expected to get %v replies, got %v", expected, n)
		}
		return nil
	})

	// We should not have received a single message on the queue sub
	// on cluster A because messages will have been delivered to
	// the member on cluster B.
	if n := atomic.LoadInt32(&qrcvOnA); n != 0 {
		t.Fatalf("Queue sub on A should not have received message, got %v", n)
	}
}

func TestNoRaceRouteMemUsage(t *testing.T) {
	oa := DefaultOptions()
	sa := RunServer(oa)
	defer sa.Shutdown()

	ob := DefaultOptions()
	ob.Routes = RoutesFromStr(fmt.Sprintf("nats://%s:%d", oa.Cluster.Host, oa.Cluster.Port))
	sb := RunServer(ob)
	defer sb.Shutdown()

	checkClusterFormed(t, sa, sb)

	responder := natsConnect(t, fmt.Sprintf("nats://%s:%d", oa.Host, oa.Port))
	defer responder.Close()
	for i := 0; i < 10; i++ {
		natsSub(t, responder, "foo", func(m *nats.Msg) {
			m.Respond(m.Data)
		})
	}
	natsFlush(t, responder)

	payload := make([]byte, 50*1024)

	bURL := fmt.Sprintf("nats://%s:%d", ob.Host, ob.Port)

	// Capture mem usage
	mem := runtime.MemStats{}
	runtime.ReadMemStats(&mem)
	inUseBefore := mem.HeapInuse

	for i := 0; i < 100; i++ {
		requestor := natsConnect(t, bURL)
		inbox := nats.NewInbox()
		sub := natsSubSync(t, requestor, inbox)
		natsPubReq(t, requestor, "foo", inbox, payload)
		for j := 0; j < 10; j++ {
			natsNexMsg(t, sub, time.Second)
		}
		requestor.Close()
	}

	runtime.GC()
	debug.FreeOSMemory()
	runtime.ReadMemStats(&mem)
	inUseNow := mem.HeapInuse
	if inUseNow > 3*inUseBefore {
		t.Fatalf("Heap in-use before was %v, now %v: too high", inUseBefore, inUseNow)
	}
}

func TestNoRaceRouteCache(t *testing.T) {
	maxPerAccountCacheSize = 20
	prunePerAccountCacheSize = 5
	closedSubsCheckInterval = 250 * time.Millisecond

	defer func() {
		maxPerAccountCacheSize = defaultMaxPerAccountCacheSize
		prunePerAccountCacheSize = defaultPrunePerAccountCacheSize
		closedSubsCheckInterval = defaultClosedSubsCheckInterval
	}()

	for _, test := range []struct {
		name     string
		useQueue bool
	}{
		{"plain_sub", false},
		{"queue_sub", true},
	} {
		t.Run(test.name, func(t *testing.T) {

			oa := DefaultOptions()
			oa.NoSystemAccount = true
			sa := RunServer(oa)
			defer sa.Shutdown()

			ob := DefaultOptions()
			ob.NoSystemAccount = true
			ob.Routes = RoutesFromStr(fmt.Sprintf("nats://%s:%d", oa.Cluster.Host, oa.Cluster.Port))
			sb := RunServer(ob)
			defer sb.Shutdown()

			checkClusterFormed(t, sa, sb)

			responder := natsConnect(t, fmt.Sprintf("nats://%s:%d", oa.Host, oa.Port))
			defer responder.Close()
			natsSub(t, responder, "foo", func(m *nats.Msg) {
				m.Respond(m.Data)
			})
			natsFlush(t, responder)

			checkExpectedSubs(t, 1, sa)
			checkExpectedSubs(t, 1, sb)

			bURL := fmt.Sprintf("nats://%s:%d", ob.Host, ob.Port)
			requestor := natsConnect(t, bURL)
			defer requestor.Close()

			ch := make(chan struct{}, 1)
			cb := func(_ *nats.Msg) {
				select {
				case ch <- struct{}{}:
				default:
				}
			}

			sendReqs := func(t *testing.T, nc *nats.Conn, count int, unsub bool) {
				t.Helper()
				for i := 0; i < count; i++ {
					inbox := nats.NewInbox()
					var sub *nats.Subscription
					if test.useQueue {
						sub = natsQueueSub(t, nc, inbox, "queue", cb)
					} else {
						sub = natsSub(t, nc, inbox, cb)
					}
					natsPubReq(t, nc, "foo", inbox, []byte("hello"))
					select {
					case <-ch:
					case <-time.After(time.Second):
						t.Fatalf("Failed to get reply")
					}
					if unsub {
						natsUnsub(t, sub)
					}
				}
			}
			sendReqs(t, requestor, maxPerAccountCacheSize+1, true)

			var route *client
			sb.mu.Lock()
			for _, r := range sb.routes {
				route = r
				break
			}
			sb.mu.Unlock()

			checkExpected := func(t *testing.T, expected int) {
				t.Helper()
				checkFor(t, 2*time.Second, 15*time.Millisecond, func() error {
					route.mu.Lock()
					n := len(route.in.pacache)
					route.mu.Unlock()
					if n != expected {
						return fmt.Errorf("Expected %v subs in the cache, got %v", expected, n)
					}
					return nil
				})
			}
			checkExpected(t, (maxPerAccountCacheSize+1)-(prunePerAccountCacheSize+1))

			// Wait for more than the orphan check
			time.Sleep(2 * closedSubsCheckInterval)

			// Add a new subs up to point where new prune would occur
			sendReqs(t, requestor, prunePerAccountCacheSize+1, false)

			// Now closed subs should have been removed, so expected
			// subs in the cache should be the new ones.
			checkExpected(t, prunePerAccountCacheSize+1)

			// Now try wil implicit unsubscribe (due to connection close)
			sendReqs(t, requestor, maxPerAccountCacheSize+1, false)
			requestor.Close()

			checkExpected(t, maxPerAccountCacheSize-prunePerAccountCacheSize)

			// Wait for more than the orphan check
			time.Sleep(2 * closedSubsCheckInterval)

			// Now create new connection and send prunePerAccountCacheSize+1
			// and that should cause all subs from previous connection to be
			// removed from cache
			requestor = natsConnect(t, bURL)
			defer requestor.Close()

			sendReqs(t, requestor, prunePerAccountCacheSize+1, false)
			checkExpected(t, prunePerAccountCacheSize+1)
		})
	}
}

func TestNoRaceFetchAccountDoesNotRegisterAccountTwice(t *testing.T) {
	sa, oa, sb, ob, _ := runTrustedGateways(t)
	defer sa.Shutdown()
	defer sb.Shutdown()

	// Let's create a user account.
	okp, _ := nkeys.FromSeed(oSeed)
	akp, _ := nkeys.CreateAccount()
	pub, _ := akp.PublicKey()
	nac := jwt.NewAccountClaims(pub)
	jwt, _ := nac.Encode(okp)
	userAcc := pub

	// Replace B's account resolver with one that introduces
	// delay during the Fetch()
	sac := &slowAccResolver{AccountResolver: sb.AccountResolver()}
	sb.SetAccountResolver(sac)

	// Add the account in sa and sb
	addAccountToMemResolver(sa, userAcc, jwt)
	addAccountToMemResolver(sb, userAcc, jwt)

	// Tell the slow account resolver which account to slow down
	sac.Lock()
	sac.acc = userAcc
	sac.Unlock()

	urlA := fmt.Sprintf("nats://%s:%d", oa.Host, oa.Port)
	urlB := fmt.Sprintf("nats://%s:%d", ob.Host, ob.Port)

	nca, err := nats.Connect(urlA, createUserCreds(t, sa, akp))
	if err != nil {
		t.Fatalf("Error connecting to A: %v", err)
	}
	defer nca.Close()

	// Since there is an optimistic send, this message will go to B
	// and on processing this message, B will lookup/fetch this
	// account, which can produce race with the fetch of this
	// account from A's system account that sent a notification
	// about this account, or with the client connect just after
	// that.
	nca.Publish("foo", []byte("hello"))

	// Now connect and create a subscription on B
	ncb, err := nats.Connect(urlB, createUserCreds(t, sb, akp))
	if err != nil {
		t.Fatalf("Error connecting to A: %v", err)
	}
	defer ncb.Close()
	sub, err := ncb.SubscribeSync("foo")
	if err != nil {
		t.Fatalf("Error on subscribe: %v", err)
	}
	ncb.Flush()

	// Now send messages from A and B should ultimately start to receive
	// them (once the subscription has been correctly registered)
	ok := false
	for i := 0; i < 10; i++ {
		nca.Publish("foo", []byte("hello"))
		if _, err := sub.NextMsg(100 * time.Millisecond); err != nil {
			continue
		}
		ok = true
		break
	}
	if !ok {
		t.Fatalf("B should be able to receive messages")
	}

	checkTmpAccounts := func(t *testing.T, s *Server) {
		t.Helper()
		empty := true
		s.tmpAccounts.Range(func(_, _ interface{}) bool {
			empty = false
			return false
		})
		if !empty {
			t.Fatalf("tmpAccounts is not empty")
		}
	}
	checkTmpAccounts(t, sa)
	checkTmpAccounts(t, sb)
}

func TestNoRaceWriteDeadline(t *testing.T) {
	opts := DefaultOptions()
	opts.NoSystemAccount = true
	opts.WriteDeadline = 30 * time.Millisecond
	s := RunServer(opts)
	defer s.Shutdown()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port), 3*time.Second)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("CONNECT {}\r\nPING\r\nSUB foo 1\r\n")); err != nil {
		t.Fatalf("Error sending protocols to server: %v", err)
	}
	// Reduce socket buffer to increase reliability of getting
	// write deadline errors.
	c.(*net.TCPConn).SetReadBuffer(4)

	url := fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port)
	sender, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sender.Close()

	payload := make([]byte, 1000000)
	total := 1000
	for i := 0; i < total; i++ {
		if err := sender.Publish("foo", payload); err != nil {
			t.Fatalf("Error on publish: %v", err)
		}
	}
	// Flush sender connection to ensure that all data has been sent.
	if err := sender.Flush(); err != nil {
		t.Fatalf("Error on flush: %v", err)
	}

	// At this point server should have closed connection c.

	// On certain platforms, it may take more than one call before
	// getting the error.
	for i := 0; i < 100; i++ {
		if _, err := c.Write([]byte("PUB bar 5\r\nhello\r\n")); err != nil {
			// ok
			return
		}
	}
	t.Fatal("Connection should have been closed")
}

func TestNoRaceLeafNodeClusterNameConflictDeadlock(t *testing.T) {
	o := DefaultOptions()
	o.LeafNode.Port = -1
	s := RunServer(o)
	defer s.Shutdown()

	u, err := url.Parse(fmt.Sprintf("nats://127.0.0.1:%d", o.LeafNode.Port))
	if err != nil {
		t.Fatalf("Error parsing url: %v", err)
	}

	o1 := DefaultOptions()
	o1.ServerName = "A1"
	o1.Cluster.Name = "clusterA"
	o1.LeafNode.Remotes = []*RemoteLeafOpts{{URLs: []*url.URL{u}}}
	s1 := RunServer(o1)
	defer s1.Shutdown()

	checkLeafNodeConnected(t, s1)

	o2 := DefaultOptions()
	o2.ServerName = "A2"
	o2.Cluster.Name = "clusterA"
	o2.Routes = RoutesFromStr(fmt.Sprintf("nats://127.0.0.1:%d", o1.Cluster.Port))
	o2.LeafNode.Remotes = []*RemoteLeafOpts{{URLs: []*url.URL{u}}}
	s2 := RunServer(o2)
	defer s2.Shutdown()

	checkLeafNodeConnected(t, s2)
	checkClusterFormed(t, s1, s2)

	o3 := DefaultOptions()
	o3.ServerName = "A3"
	o3.Cluster.Name = "" // intentionally not set
	o3.Routes = RoutesFromStr(fmt.Sprintf("nats://127.0.0.1:%d", o1.Cluster.Port))
	o3.LeafNode.Remotes = []*RemoteLeafOpts{{URLs: []*url.URL{u}}}
	s3 := RunServer(o3)
	defer s3.Shutdown()

	checkLeafNodeConnected(t, s3)
	checkClusterFormed(t, s1, s2, s3)
}

// This test is same than TestAccountAddServiceImportRace but running
// without the -race flag, it would capture more easily the possible
// duplicate sid, resulting in less than expected number of subscriptions
// in the account's internal subscriptions map.
func TestNoRaceAccountAddServiceImportRace(t *testing.T) {
	TestAccountAddServiceImportRace(t)
}

// Similar to the routed version. Make sure we receive all of the
// messages with auto-unsubscribe enabled.
func TestNoRaceQueueAutoUnsubscribe(t *testing.T) {
	opts := DefaultOptions()
	s := RunServer(opts)
	defer s.Shutdown()

	nc, err := nats.Connect(fmt.Sprintf("nats://%s:%d", opts.Host, opts.Port))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer nc.Close()

	rbar := int32(0)
	barCb := func(m *nats.Msg) {
		atomic.AddInt32(&rbar, 1)
	}
	rbaz := int32(0)
	bazCb := func(m *nats.Msg) {
		atomic.AddInt32(&rbaz, 1)
	}

	// Create 1000 subscriptions with auto-unsubscribe of 1.
	// Do two groups, one bar and one baz.
	total := 1000
	for i := 0; i < total; i++ {
		qsub, err := nc.QueueSubscribe("foo", "bar", barCb)
		if err != nil {
			t.Fatalf("Error on subscribe: %v", err)
		}
		if err := qsub.AutoUnsubscribe(1); err != nil {
			t.Fatalf("Error on auto-unsubscribe: %v", err)
		}
		qsub, err = nc.QueueSubscribe("foo", "baz", bazCb)
		if err != nil {
			t.Fatalf("Error on subscribe: %v", err)
		}
		if err := qsub.AutoUnsubscribe(1); err != nil {
			t.Fatalf("Error on auto-unsubscribe: %v", err)
		}
	}
	nc.Flush()

	expected := int32(total)
	for i := int32(0); i < expected; i++ {
		nc.Publish("foo", []byte("Don't Drop Me!"))
	}
	nc.Flush()

	checkFor(t, 5*time.Second, 10*time.Millisecond, func() error {
		nbar := atomic.LoadInt32(&rbar)
		nbaz := atomic.LoadInt32(&rbaz)
		if nbar == expected && nbaz == expected {
			return nil
		}
		return fmt.Errorf("Did not receive all %d queue messages, received %d for 'bar' and %d for 'baz'",
			expected, atomic.LoadInt32(&rbar), atomic.LoadInt32(&rbaz))
	})
}

func TestNoRaceAcceptLoopsDoNotLeaveOpenedConn(t *testing.T) {
	for _, test := range []struct {
		name string
		url  func(o *Options) (string, int)
	}{
		{"client", func(o *Options) (string, int) { return o.Host, o.Port }},
		{"route", func(o *Options) (string, int) { return o.Cluster.Host, o.Cluster.Port }},
		{"gateway", func(o *Options) (string, int) { return o.Gateway.Host, o.Gateway.Port }},
		{"leafnode", func(o *Options) (string, int) { return o.LeafNode.Host, o.LeafNode.Port }},
		{"websocket", func(o *Options) (string, int) { return o.Websocket.Host, o.Websocket.Port }},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := DefaultOptions()
			o.DisableShortFirstPing = true
			o.Accounts = []*Account{NewAccount("$SYS")}
			o.SystemAccount = "$SYS"
			o.Cluster.Name = "abc"
			o.Cluster.Host = "127.0.0.1"
			o.Cluster.Port = -1
			o.Gateway.Name = "abc"
			o.Gateway.Host = "127.0.0.1"
			o.Gateway.Port = -1
			o.LeafNode.Host = "127.0.0.1"
			o.LeafNode.Port = -1
			o.Websocket.Host = "127.0.0.1"
			o.Websocket.Port = -1
			o.Websocket.HandshakeTimeout = 1
			o.Websocket.NoTLS = true
			s := RunServer(o)
			defer s.Shutdown()

			host, port := test.url(o)
			url := fmt.Sprintf("%s:%d", host, port)
			var conns []net.Conn

			wg := sync.WaitGroup{}
			wg.Add(1)
			done := make(chan struct{}, 1)
			go func() {
				defer wg.Done()
				// Have an upper limit
				for i := 0; i < 200; i++ {
					c, err := net.Dial("tcp", url)
					if err != nil {
						return
					}
					conns = append(conns, c)
					select {
					case <-done:
						return
					default:
					}
				}
			}()
			time.Sleep(15 * time.Millisecond)
			s.Shutdown()
			close(done)
			wg.Wait()
			for _, c := range conns {
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				br := bufio.NewReader(c)
				// Read INFO for connections that were accepted
				_, _, err := br.ReadLine()
				if err == nil {
					// After that, the connection should be closed,
					// so we should get an error here.
					_, _, err = br.ReadLine()
				}
				// We expect an io.EOF or any other error indicating the use of a closed
				// connection, but we should not get the timeout error.
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					err = nil
				}
				if err == nil {
					var buf [10]byte
					c.SetDeadline(time.Now().Add(2 * time.Second))
					c.Write([]byte("C"))
					_, err = c.Read(buf[:])
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						err = nil
					}
				}
				if err == nil {
					t.Fatalf("Connection should have been closed")
				}
				c.Close()
			}
		})
	}
}

func TestNoRaceJetStreamDeleteStreamManyConsumers(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	mname := "MYS"
	mset, err := s.GlobalAccount().addStream(&StreamConfig{Name: mname, Storage: FileStorage})
	if err != nil {
		t.Fatalf("Unexpected error adding stream: %v", err)
	}

	// This number needs to be higher than the internal sendq size to trigger what this test is testing.
	for i := 0; i < 2000; i++ {
		_, err := mset.addConsumer(&ConsumerConfig{
			Durable:        fmt.Sprintf("D-%d", i),
			DeliverSubject: fmt.Sprintf("deliver.%d", i),
		})
		if err != nil {
			t.Fatalf("Error creating consumer: %v", err)
		}
	}
	// With bug this would not return and would hang.
	mset.delete()
}

// We used to swap accounts on an inbound message when processing service imports.
// Until JetStream this was kinda ok, but with JetStream we can have pull consumers
// trying to access the clients account in another Go routine now which causes issues.
// This is not limited to the case above, its just the one that exposed it.
// This test is to show that issue and that the fix works, meaning we no longer swap c.acc.
func TestNoRaceJetStreamServiceImportAccountSwapIssue(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	sub, err := js.PullSubscribe("foo", "dlc")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	beforeSubs := s.NumSubscriptions()

	// How long we want both sides to run.
	timeout := time.Now().Add(3 * time.Second)
	errs := make(chan error, 1)

	// Publishing side, which will signal the consumer that is waiting and which will access c.acc. If publish
	// operation runs concurrently we will catch c.acc being $SYS some of the time.
	go func() {
		time.Sleep(100 * time.Millisecond)
		for time.Now().Before(timeout) {
			// This will signal the delivery of the pull messages.
			js.Publish("foo", []byte("Hello"))
			// This will swap the account because of JetStream service import.
			// We can get an error here with the bug or not.
			if _, err := js.StreamInfo("TEST"); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()

	// Pull messages flow.
	var received int
	for time.Now().Before(timeout) {
		if msgs, err := sub.Fetch(1, nats.MaxWait(200*time.Millisecond)); err == nil {
			for _, m := range msgs {
				received++
				m.AckSync()
			}
		} else {
			break
		}
	}
	// Wait on publisher Go routine and check for errors.
	if err := <-errs; err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Double check all received.
	si, err := js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if int(si.State.Msgs) != received {
		t.Fatalf("Expected to receive %d msgs, only got %d", si.State.Msgs, received)
	}
	// Now check for leaked subs from the fetch call above. That is what we first saw from the bug.
	if afterSubs := s.NumSubscriptions(); afterSubs != beforeSubs {
		t.Fatalf("Leaked subscriptions: %d before, %d after", beforeSubs, afterSubs)
	}
}

func TestNoRaceJetStreamAPIStreamListPaging(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	// Create 2X limit
	streamsNum := 2 * JSApiNamesLimit
	for i := 1; i <= streamsNum; i++ {
		name := fmt.Sprintf("STREAM-%06d", i)
		cfg := StreamConfig{Name: name, Storage: MemoryStorage}
		_, err := s.GlobalAccount().addStream(&cfg)
		if err != nil {
			t.Fatalf("Unexpected error adding stream: %v", err)
		}
	}

	// Client for API requests.
	nc := clientConnectToServer(t, s)
	defer nc.Close()

	reqList := func(offset int) []byte {
		t.Helper()
		var req []byte
		if offset > 0 {
			req, _ = json.Marshal(&ApiPagedRequest{Offset: offset})
		}
		resp, err := nc.Request(JSApiStreams, req, time.Second)
		if err != nil {
			t.Fatalf("Unexpected error getting stream list: %v", err)
		}
		return resp.Data
	}

	checkResp := func(resp []byte, expectedLen, expectedOffset int) {
		t.Helper()
		var listResponse JSApiStreamNamesResponse
		if err := json.Unmarshal(resp, &listResponse); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(listResponse.Streams) != expectedLen {
			t.Fatalf("Expected only %d streams but got %d", expectedLen, len(listResponse.Streams))
		}
		if listResponse.Total != streamsNum {
			t.Fatalf("Expected total to be %d but got %d", streamsNum, listResponse.Total)
		}
		if listResponse.Offset != expectedOffset {
			t.Fatalf("Expected offset to be %d but got %d", expectedOffset, listResponse.Offset)
		}
		if expectedLen < 1 {
			return
		}
		// Make sure we get the right stream.
		sname := fmt.Sprintf("STREAM-%06d", expectedOffset+1)
		if listResponse.Streams[0] != sname {
			t.Fatalf("Expected stream %q to be first, got %q", sname, listResponse.Streams[0])
		}
	}

	checkResp(reqList(0), JSApiNamesLimit, 0)
	checkResp(reqList(JSApiNamesLimit), JSApiNamesLimit, JSApiNamesLimit)
	checkResp(reqList(streamsNum), 0, streamsNum)
	checkResp(reqList(streamsNum-22), 22, streamsNum-22)
	checkResp(reqList(streamsNum+22), 0, streamsNum)
}

func TestNoRaceJetStreamAPIConsumerListPaging(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	sname := "MYSTREAM"
	mset, err := s.GlobalAccount().addStream(&StreamConfig{Name: sname})
	if err != nil {
		t.Fatalf("Unexpected error adding stream: %v", err)
	}

	// Client for API requests.
	nc := clientConnectToServer(t, s)
	defer nc.Close()

	consumersNum := JSApiNamesLimit
	for i := 1; i <= consumersNum; i++ {
		dsubj := fmt.Sprintf("d.%d", i)
		sub, _ := nc.SubscribeSync(dsubj)
		defer sub.Unsubscribe()
		nc.Flush()

		_, err := mset.addConsumer(&ConsumerConfig{DeliverSubject: dsubj})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	reqListSubject := fmt.Sprintf(JSApiConsumersT, sname)
	reqList := func(offset int) []byte {
		t.Helper()
		var req []byte
		if offset > 0 {
			req, _ = json.Marshal(&JSApiConsumersRequest{ApiPagedRequest: ApiPagedRequest{Offset: offset}})
		}
		resp, err := nc.Request(reqListSubject, req, time.Second)
		if err != nil {
			t.Fatalf("Unexpected error getting stream list: %v", err)
		}
		return resp.Data
	}

	checkResp := func(resp []byte, expectedLen, expectedOffset int) {
		t.Helper()
		var listResponse JSApiConsumerNamesResponse
		if err := json.Unmarshal(resp, &listResponse); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if len(listResponse.Consumers) != expectedLen {
			t.Fatalf("Expected only %d streams but got %d", expectedLen, len(listResponse.Consumers))
		}
		if listResponse.Total != consumersNum {
			t.Fatalf("Expected total to be %d but got %d", consumersNum, listResponse.Total)
		}
		if listResponse.Offset != expectedOffset {
			t.Fatalf("Expected offset to be %d but got %d", expectedOffset, listResponse.Offset)
		}
	}

	checkResp(reqList(0), JSApiNamesLimit, 0)
	checkResp(reqList(consumersNum-22), 22, consumersNum-22)
	checkResp(reqList(consumersNum+22), 0, consumersNum)
}

func TestNoRaceJetStreamWorkQueueLoadBalance(t *testing.T) {
	s := RunBasicJetStreamServer()
	defer s.Shutdown()

	mname := "MY_MSG_SET"
	mset, err := s.GlobalAccount().addStream(&StreamConfig{Name: mname, Subjects: []string{"foo", "bar"}})
	if err != nil {
		t.Fatalf("Unexpected error adding message set: %v", err)
	}
	defer mset.delete()

	// Create basic work queue mode consumer.
	oname := "WQ"
	o, err := mset.addConsumer(&ConsumerConfig{Durable: oname, AckPolicy: AckExplicit})
	if err != nil {
		t.Fatalf("Expected no error with durable, got %v", err)
	}
	defer o.delete()

	// To send messages.
	nc := clientConnectToServer(t, s)
	defer nc.Close()

	// For normal work queue semantics, you send requests to the subject with stream and consumer name.
	reqMsgSubj := o.requestNextMsgSubject()

	numWorkers := 25
	counts := make([]int32, numWorkers)
	var received int32

	rwg := &sync.WaitGroup{}
	rwg.Add(numWorkers)

	wg := &sync.WaitGroup{}
	wg.Add(numWorkers)
	ch := make(chan bool)

	toSend := 1000

	for i := 0; i < numWorkers; i++ {
		nc := clientConnectToServer(t, s)
		defer nc.Close()

		go func(index int32) {
			rwg.Done()
			defer wg.Done()
			<-ch

			for counter := &counts[index]; ; {
				m, err := nc.Request(reqMsgSubj, nil, 100*time.Millisecond)
				if err != nil {
					return
				}
				m.Respond(nil)
				atomic.AddInt32(counter, 1)
				if total := atomic.AddInt32(&received, 1); total >= int32(toSend) {
					return
				}
			}
		}(int32(i))
	}

	// Wait for requestors to be ready
	rwg.Wait()
	close(ch)

	sendSubj := "bar"
	for i := 0; i < toSend; i++ {
		sendStreamMsg(t, nc, sendSubj, "Hello World!")
	}

	// Wait for test to complete.
	wg.Wait()

	target := toSend / numWorkers
	delta := target/2 + 5
	low, high := int32(target-delta), int32(target+delta)

	for i := 0; i < numWorkers; i++ {
		if msgs := atomic.LoadInt32(&counts[i]); msgs < low || msgs > high {
			t.Fatalf("Messages received for worker [%d] too far off from target of %d, got %d", i, target, msgs)
		}
	}
}

func TestNoRaceJetStreamClusterLargeStreamInlineCatchup(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "LSS", 3)
	defer c.shutdown()

	// Client based API
	s := c.randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	sr := c.randomNonStreamLeader("$G", "TEST")
	sr.Shutdown()

	// In case sr was meta leader.
	c.waitOnLeader()

	msg, toSend := []byte("Hello JS Clustering"), 5000

	// Now fill up stream.
	for i := 0; i < toSend; i++ {
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}
	si, err := js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Check active state as well, shows that the owner answered.
	if si.State.Msgs != uint64(toSend) {
		t.Fatalf("Expected %d msgs, got bad state: %+v", toSend, si.State)
	}

	// Kill our current leader to make just 2.
	c.streamLeader("$G", "TEST").Shutdown()

	// Now restart the shutdown peer and wait for it to be current.
	sr = c.restartServer(sr)
	c.waitOnStreamCurrent(sr, "$G", "TEST")

	// Ask other servers to stepdown as leader so that sr becomes the leader.
	checkFor(t, 20*time.Second, 200*time.Millisecond, func() error {
		c.waitOnStreamLeader("$G", "TEST")
		if sl := c.streamLeader("$G", "TEST"); sl != sr {
			sl.JetStreamStepdownStream("$G", "TEST")
			return fmt.Errorf("Server %s is not leader yet", sr)
		}
		return nil
	})

	si, err = js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Check that we have all of our messsages stored.
	// Wait for a bit for upper layers to process.
	checkFor(t, 2*time.Second, 100*time.Millisecond, func() error {
		if si.State.Msgs != uint64(toSend) {
			return fmt.Errorf("Expected %d msgs, got %d", toSend, si.State.Msgs)
		}
		return nil
	})
}

func TestNoRaceJetStreamClusterStreamCreateAndLostQuorum(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R5S", 3)
	defer c.shutdown()

	// Client based API
	s := c.randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	sub, err := nc.SubscribeSync(JSAdvisoryStreamQuorumLostPre + ".*")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if _, err := js.AddStream(&nats.StreamConfig{Name: "NO-LQ-START", Replicas: 3}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	c.waitOnStreamLeader("$G", "NO-LQ-START")
	checkSubsPending(t, sub, 0)

	c.stopAll()
	// Start up the one we were connected to first and wait for it to be connected.
	s = c.restartServer(s)
	nc, err = nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer nc.Close()

	sub, err = nc.SubscribeSync(JSAdvisoryStreamQuorumLostPre + ".*")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	nc.Flush()

	c.restartAll()
	c.waitOnStreamLeader("$G", "NO-LQ-START")

	checkSubsPending(t, sub, 0)
}

func TestNoRaceJetStreamSuperClusterMirrors(t *testing.T) {
	sc := createJetStreamSuperCluster(t, 3, 3)
	defer sc.shutdown()

	// Client based API
	s := sc.clusterForName("C2").randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	// Create source stream.
	_, err := js.AddStream(&nats.StreamConfig{Name: "S1", Subjects: []string{"foo", "bar"}, Replicas: 3, Placement: &nats.Placement{Cluster: "C2"}})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Needed while Go client does not have mirror support.
	createStream := func(cfg *nats.StreamConfig) {
		t.Helper()
		if _, err := js.AddStream(cfg); err != nil {
			t.Fatalf("Unexpected error: %+v", err)
		}
	}

	// Send 100 messages.
	for i := 0; i < 100; i++ {
		if _, err := js.Publish("foo", []byte("MIRRORS!")); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	createStream(&nats.StreamConfig{
		Name:      "M1",
		Mirror:    &nats.StreamSource{Name: "S1"},
		Placement: &nats.Placement{Cluster: "C1"},
	})

	checkFor(t, 2*time.Second, 100*time.Millisecond, func() error {
		si, err := js.StreamInfo("M1")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.Msgs != 100 {
			return fmt.Errorf("Expected 100 msgs, got state: %+v", si.State)
		}
		return nil
	})

	// Purge the source stream.
	if err := js.PurgeStream("S1"); err != nil {
		t.Fatalf("Unexpected purge error: %v", err)
	}
	// Send 50 more msgs now.
	for i := 0; i < 50; i++ {
		if _, err := js.Publish("bar", []byte("OK")); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	createStream(&nats.StreamConfig{
		Name:      "M2",
		Mirror:    &nats.StreamSource{Name: "S1"},
		Replicas:  3,
		Placement: &nats.Placement{Cluster: "C3"},
	})

	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		si, err := js.StreamInfo("M2")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.Msgs != 50 {
			return fmt.Errorf("Expected 50 msgs, got state: %+v", si.State)
		}
		if si.State.FirstSeq != 101 {
			return fmt.Errorf("Expected start seq of 101, got state: %+v", si.State)
		}
		return nil
	})

	sl := sc.clusterForName("C3").streamLeader("$G", "M2")
	doneCh := make(chan bool)

	// Now test that if the mirror get's interrupted that it picks up where it left off etc.
	go func() {
		// Send 100 more messages.
		for i := 0; i < 100; i++ {
			if _, err := js.Publish("foo", []byte("MIRRORS!")); err != nil {
				t.Errorf("Unexpected publish on %d error: %v", i, err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		doneCh <- true
	}()

	time.Sleep(20 * time.Millisecond)
	sl.Shutdown()

	<-doneCh
	sc.clusterForName("C3").waitOnStreamLeader("$G", "M2")

	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		si, err := js.StreamInfo("M2")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.Msgs != 150 {
			return fmt.Errorf("Expected 150 msgs, got state: %+v", si.State)
		}
		if si.State.FirstSeq != 101 {
			return fmt.Errorf("Expected start seq of 101, got state: %+v", si.State)
		}
		return nil
	})
}

func TestNoRaceJetStreamSuperClusterMixedModeMirrors(t *testing.T) {
	// Unlike the similar sources test, this test is not reliably catching the bug
	// that would cause mirrors to not have the expected messages count.
	// Still, adding this test in case we have a regression and we are lucky in
	// getting the failure while running this.

	tmpl := `
		listen: 127.0.0.1:-1
		server_name: %s
		jetstream: { domain: ngs, max_mem_store: 256MB, max_file_store: 2GB, store_dir: '%s'}
		leaf: { listen: 127.0.0.1:-1 }

		cluster {
			name: %s
			listen: 127.0.0.1:%d
			routes = [%s]
		}

		accounts { $SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] } }
	`
	sc := createJetStreamSuperClusterWithTemplateAndModHook(t, tmpl, 7, 4,
		func(serverName, clusterName, storeDir, conf string) string {
			sname := serverName[strings.Index(serverName, "-")+1:]
			switch sname {
			case "S5", "S6", "S7":
				conf = strings.ReplaceAll(conf, "jetstream: { ", "#jetstream: { ")
			default:
				conf = strings.ReplaceAll(conf, "leaf: { ", "#leaf: { ")
			}
			return conf
		})
	defer sc.shutdown()

	// Connect our client to a non JS server
	c := sc.randomCluster()
	var s *Server
	for s == nil {
		if as := c.randomServer(); !as.JetStreamEnabled() {
			s = as
			break
		}
	}
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	toSend := 1000
	// Create 10 origin streams
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("S%d", i+1)
		if _, err := js.AddStream(&nats.StreamConfig{Name: name}); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		// Load them up with a bunch of messages.
		for n := 0; n < toSend; n++ {
			m := nats.NewMsg(name)
			m.Header.Set("stream", name)
			m.Header.Set("idx", strconv.FormatInt(int64(n+1), 10))
			if err := nc.PublishMsg(m); err != nil {
				t.Fatalf("Unexpected publish error: %v", err)
			}
		}
	}

	for i := 0; i < 3; i++ {
		// Now create our mirrors
		wg := sync.WaitGroup{}
		mirrorsCount := 10
		wg.Add(mirrorsCount)
		errCh := make(chan error, 1)
		for m := 0; m < mirrorsCount; m++ {
			sname := fmt.Sprintf("S%d", rand.Intn(10)+1)
			go func(sname string, mirrorIdx int) {
				defer wg.Done()
				if _, err := js.AddStream(&nats.StreamConfig{
					Name:     fmt.Sprintf("M%d", mirrorIdx),
					Mirror:   &nats.StreamSource{Name: sname},
					Replicas: 3,
				}); err != nil {
					select {
					case errCh <- err:
					default:
					}
				}
			}(sname, m+1)
		}
		wg.Wait()
		select {
		case err := <-errCh:
			t.Fatalf("Error creating mirrors: %v", err)
		default:
		}
		// Now check the mirrors have all expected messages
		for m := 0; m < mirrorsCount; m++ {
			name := fmt.Sprintf("M%d", m+1)
			checkFor(t, 15*time.Second, 500*time.Millisecond, func() error {
				si, err := js.StreamInfo(name)
				if err != nil {
					t.Fatalf("Could not retrieve stream info")
				}
				if si.State.Msgs != uint64(toSend) {
					return fmt.Errorf("Expected %d msgs, got state: %+v", toSend, si.State)
				}
				return nil
			})
			err := js.DeleteStream(name)
			require_NoError(t, err)
		}
	}
}

func TestNoRaceJetStreamSuperClusterSources(t *testing.T) {

	sc := createJetStreamSuperCluster(t, 3, 3)
	defer sc.shutdown()

	// Client based API
	s := sc.clusterForName("C1").randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	// Create our source streams.
	for _, sname := range []string{"foo", "bar", "baz"} {
		if _, err := js.AddStream(&nats.StreamConfig{Name: sname, Replicas: 1}); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	sendBatch := func(subject string, n int) {
		for i := 0; i < n; i++ {
			msg := fmt.Sprintf("MSG-%d", i+1)
			if _, err := js.Publish(subject, []byte(msg)); err != nil {
				t.Fatalf("Unexpected publish error: %v", err)
			}
		}
	}
	// Populate each one.
	sendBatch("foo", 10)
	sendBatch("bar", 15)
	sendBatch("baz", 25)

	// Needed while Go client does not have mirror support for creating mirror or source streams.
	createStream := func(cfg *nats.StreamConfig) {
		t.Helper()
		if _, err := js.AddStream(cfg); err != nil {
			t.Fatalf("Unexpected error: %+v", err)
		}
	}

	cfg := &nats.StreamConfig{
		Name: "MS",
		Sources: []*nats.StreamSource{
			{Name: "foo"},
			{Name: "bar"},
			{Name: "baz"},
		},
	}

	createStream(cfg)
	time.Sleep(time.Second)

	// Faster timeout since we loop below checking for condition.
	js2, err := nc.JetStream(nats.MaxWait(50 * time.Millisecond))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	checkFor(t, 10*time.Second, 100*time.Millisecond, func() error {
		si, err := js2.StreamInfo("MS")
		if err != nil {
			return err
		}
		if si.State.Msgs != 50 {
			return fmt.Errorf("Expected 50 msgs, got state: %+v", si.State)
		}
		return nil
	})

	// Purge the source streams.
	for _, sname := range []string{"foo", "bar", "baz"} {
		if err := js.PurgeStream(sname); err != nil {
			t.Fatalf("Unexpected purge error: %v", err)
		}
	}

	if err := js.DeleteStream("MS"); err != nil {
		t.Fatalf("Unexpected delete error: %v", err)
	}

	// Send more msgs now.
	sendBatch("foo", 10)
	sendBatch("bar", 15)
	sendBatch("baz", 25)

	cfg = &nats.StreamConfig{
		Name: "MS2",
		Sources: []*nats.StreamSource{
			{Name: "foo"},
			{Name: "bar"},
			{Name: "baz"},
		},
		Replicas:  3,
		Placement: &nats.Placement{Cluster: "C3"},
	}

	createStream(cfg)

	checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
		si, err := js2.StreamInfo("MS2")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.Msgs != 50 {
			return fmt.Errorf("Expected 50 msgs, got state: %+v", si.State)
		}
		if si.State.FirstSeq != 1 {
			return fmt.Errorf("Expected start seq of 1, got state: %+v", si.State)
		}
		return nil
	})

	sl := sc.clusterForName("C3").streamLeader("$G", "MS2")
	doneCh := make(chan bool)

	if sl == sc.leader() {
		nc.Request(JSApiLeaderStepDown, nil, time.Second)
		sc.waitOnLeader()
	}

	// Now test that if the mirror get's interrupted that it picks up where it left off etc.
	go func() {
		// Send 50 more messages each.
		for i := 0; i < 50; i++ {
			msg := fmt.Sprintf("R-MSG-%d", i+1)
			for _, sname := range []string{"foo", "bar", "baz"} {
				m := nats.NewMsg(sname)
				m.Header.Set(nats.MsgIdHdr, sname+"-"+msg)
				m.Data = []byte(msg)
				if _, err := js.PublishMsg(m); err != nil {
					t.Errorf("Unexpected publish error: %v", err)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		doneCh <- true
	}()

	time.Sleep(20 * time.Millisecond)
	sl.Shutdown()

	sc.clusterForName("C3").waitOnStreamLeader("$G", "MS2")
	<-doneCh

	checkFor(t, 15*time.Second, 100*time.Millisecond, func() error {
		si, err := js2.StreamInfo("MS2")
		if err != nil {
			return err
		}
		if si.State.Msgs != 200 {
			return fmt.Errorf("Expected 200 msgs, got state: %+v", si.State)
		}
		return nil
	})
}

func TestNoRaceJetStreamClusterSourcesMuxd(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "SMUX", 3)
	defer c.shutdown()

	// Client for API requests.
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	// Send in 10000 messages.
	msg, toSend := make([]byte, 1024), 10000
	rand.Read(msg)

	var sources []*nats.StreamSource
	// Create 10 origin streams.
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("O-%d", i)
		if _, err := js.AddStream(&nats.StreamConfig{Name: name}); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		// Load them up with a bunch of messages.
		for n := 0; n < toSend; n++ {
			if err := nc.Publish(name, msg); err != nil {
				t.Fatalf("Unexpected publish error: %v", err)
			}
		}
		sources = append(sources, &nats.StreamSource{Name: name})
	}

	// Now create our downstream stream that sources from all of them.
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Replicas: 2, Sources: sources}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	checkFor(t, 20*time.Second, 500*time.Millisecond, func() error {
		si, err := js.StreamInfo("S")
		if err != nil {
			t.Fatalf("Could not retrieve stream info")
		}
		if si.State.Msgs != uint64(10*toSend) {
			return fmt.Errorf("Expected %d msgs, got state: %+v", toSend*10, si.State)
		}
		return nil
	})

}

func TestNoRaceJetStreamSuperClusterMixedModeSources(t *testing.T) {
	tmpl := `
		listen: 127.0.0.1:-1
		server_name: %s
		jetstream: { domain: ngs, max_mem_store: 256MB, max_file_store: 2GB, store_dir: '%s'}
		leaf: { listen: 127.0.0.1:-1 }

		cluster {
			name: %s
			listen: 127.0.0.1:%d
			routes = [%s]
		}

		accounts { $SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] } }
	`
	sc := createJetStreamSuperClusterWithTemplateAndModHook(t, tmpl, 7, 2,
		func(serverName, clusterName, storeDir, conf string) string {
			sname := serverName[strings.Index(serverName, "-")+1:]
			switch sname {
			case "S5", "S6", "S7":
				conf = strings.ReplaceAll(conf, "jetstream: { ", "#jetstream: { ")
			default:
				conf = strings.ReplaceAll(conf, "leaf: { ", "#leaf: { ")
			}
			return conf
		})
	defer sc.shutdown()

	// Connect our client to a non JS server
	c := sc.randomCluster()
	var s *Server
	for s == nil {
		if as := c.randomServer(); !as.JetStreamEnabled() {
			s = as
			break
		}
	}
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	toSend := 1000
	var sources []*nats.StreamSource
	// Create 100 origin streams.
	for i := 1; i <= 100; i++ {
		name := fmt.Sprintf("O-%d", i)
		if _, err := js.AddStream(&nats.StreamConfig{Name: name}); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		// Load them up with a bunch of messages.
		for n := 0; n < toSend; n++ {
			m := nats.NewMsg(name)
			m.Header.Set("stream", name)
			m.Header.Set("idx", strconv.FormatInt(int64(n+1), 10))
			if err := nc.PublishMsg(m); err != nil {
				t.Fatalf("Unexpected publish error: %v", err)
			}
		}
		sources = append(sources, &nats.StreamSource{Name: name})
	}

	for i := 0; i < 3; i++ {
		// Now create our downstream stream that sources from all of them.
		if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Replicas: 3, Sources: sources}); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		checkFor(t, 15*time.Second, 1000*time.Millisecond, func() error {
			si, err := js.StreamInfo("S")
			if err != nil {
				t.Fatalf("Could not retrieve stream info")
			}
			if si.State.Msgs != uint64(100*toSend) {
				return fmt.Errorf("Expected %d msgs, got state: %+v", toSend*100, si.State)
			}
			return nil
		})

		err := js.DeleteStream("S")
		require_NoError(t, err)
	}
}

func TestNoRaceJetStreamClusterExtendedStreamPurgeStall(t *testing.T) {
	// Uncomment to run. Needs to be on a big machine. Do not want as part of Travis tests atm.
	skip(t)

	cerr := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexepected err: %s", err)
		}
	}

	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	si, err := js.AddStream(&nats.StreamConfig{
		Name:     "KV",
		Subjects: []string{"kv.>"},
		Storage:  nats.FileStorage,
	})
	cerr(t, err)

	// 100kb messages spread over 1000 different subjects
	body := make([]byte, 100*1024)
	for i := 0; i < 50000; i++ {
		if _, err := js.PublishAsync(fmt.Sprintf("kv.%d", i%1000), body); err != nil {
			cerr(t, err)
		}
	}
	checkFor(t, 5*time.Second, 200*time.Millisecond, func() error {
		if si, err = js.StreamInfo("KV"); err != nil {
			return err
		}
		if si.State.Msgs == 50000 {
			return nil
		}
		return fmt.Errorf("waiting for more")
	})

	jp, _ := json.Marshal(&JSApiStreamPurgeRequest{Subject: "kv.20"})
	start := time.Now()
	res, err := nc.Request(fmt.Sprintf(JSApiStreamPurgeT, "KV"), jp, time.Minute)
	elapsed := time.Since(start)
	cerr(t, err)
	pres := JSApiStreamPurgeResponse{}
	err = json.Unmarshal(res.Data, &pres)
	cerr(t, err)
	if !pres.Success {
		t.Fatalf("purge failed: %#v", pres)
	}
	if elapsed > time.Second {
		t.Fatalf("Purge took too long %s", elapsed)
	}
	v, _ := s.Varz(nil)
	if v.Mem > 100*1024*1024 { // 100MB limit but in practice < 100MB -> Was ~7GB when failing.
		t.Fatalf("Used too much memory: %v", friendlyBytes(v.Mem))
	}
}

func TestNoRaceJetStreamClusterMirrorExpirationAndMissingSequences(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "MMS", 9)
	defer c.shutdown()

	// Client for API requests.
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	sendBatch := func(n int) {
		t.Helper()
		// Send a batch to a given subject.
		for i := 0; i < n; i++ {
			if _, err := js.Publish("TEST", []byte("OK")); err != nil {
				t.Fatalf("Unexpected publish error: %v", err)
			}
		}
	}

	checkStream := func(stream string, num uint64) {
		t.Helper()
		checkFor(t, 10*time.Second, 50*time.Millisecond, func() error {
			si, err := js.StreamInfo(stream)
			if err != nil {
				return err
			}
			if si.State.Msgs != num {
				return fmt.Errorf("Expected %d msgs, got %d", num, si.State.Msgs)
			}
			return nil
		})
	}

	checkMirror := func(num uint64) { t.Helper(); checkStream("M", num) }
	checkTest := func(num uint64) { t.Helper(); checkStream("TEST", num) }

	// Origin
	_, err := js.AddStream(&nats.StreamConfig{
		Name:   "TEST",
		MaxAge: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	ts := c.streamLeader("$G", "TEST")
	ml := c.leader()

	// Create mirror now.
	for ms := ts; ms == ts || ms == ml; {
		_, err = js.AddStream(&nats.StreamConfig{
			Name:     "M",
			Mirror:   &nats.StreamSource{Name: "TEST"},
			Replicas: 2,
		})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		ms = c.streamLeader("$G", "M")
		if ts == ms || ms == ml {
			// Delete and retry.
			js.DeleteStream("M")
		}
	}

	sendBatch(10)
	checkMirror(10)

	// Now shutdown the server with the mirror.
	ms := c.streamLeader("$G", "M")
	ms.Shutdown()
	c.waitOnLeader()

	// Send more messages but let them expire.
	sendBatch(10)
	checkTest(0)

	c.restartServer(ms)
	c.checkClusterFormed()
	c.waitOnStreamLeader("$G", "M")

	sendBatch(10)
	checkMirror(20)
}

func TestNoRaceLargeActiveOnReplica(t *testing.T) {
	// Uncomment to run.
	skip(t)

	c := createJetStreamClusterExplicit(t, "LAG", 3)
	defer c.shutdown()

	// Client for API requests.
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	timeout := time.Now().Add(60 * time.Second)
	for time.Now().Before(timeout) {
		si, err := js.AddStream(&nats.StreamConfig{
			Name:     "TEST",
			Subjects: []string{"foo", "bar"},
			Replicas: 3,
		})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		for _, r := range si.Cluster.Replicas {
			if r.Active > 5*time.Second {
				t.Fatalf("Bad Active value: %+v", r)
			}
		}
		if err := js.DeleteStream("TEST"); err != nil {
			t.Fatalf("Unexpected delete error: %v", err)
		}
	}
}

func TestNoRaceJetStreamSuperClusterRIPStress(t *testing.T) {
	// Uncomment to run. Needs to be on a big machine.
	skip(t)

	sc := createJetStreamSuperCluster(t, 3, 3)
	defer sc.shutdown()

	// Client based API
	s := sc.clusterForName("C2").randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	scm := make(map[string][]string)

	// Create 50 streams per cluster.
	for _, cn := range []string{"C1", "C2", "C3"} {
		var streams []string
		for i := 0; i < 50; i++ {
			sn := fmt.Sprintf("%s-S%d", cn, i+1)
			streams = append(streams, sn)
			_, err := js.AddStream(&nats.StreamConfig{
				Name:      sn,
				Replicas:  3,
				Placement: &nats.Placement{Cluster: cn},
				MaxAge:    2 * time.Minute,
				MaxMsgs:   50_000,
			})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		}
		scm[cn] = streams
	}

	sourceForCluster := func(cn string) []*nats.StreamSource {
		var sns []string
		switch cn {
		case "C1":
			sns = scm["C2"]
		case "C2":
			sns = scm["C3"]
		case "C3":
			sns = scm["C1"]
		default:
			t.Fatalf("Unknown cluster %q", cn)
		}
		var ss []*nats.StreamSource
		for _, sn := range sns {
			ss = append(ss, &nats.StreamSource{Name: sn})
		}
		return ss
	}

	// Mux all 50 streams from one cluster to a single stream across a GW connection to another cluster.
	_, err := js.AddStream(&nats.StreamConfig{
		Name:      "C1-S-MUX",
		Replicas:  2,
		Placement: &nats.Placement{Cluster: "C1"},
		Sources:   sourceForCluster("C2"),
		MaxAge:    time.Minute,
		MaxMsgs:   20_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "C2-S-MUX",
		Replicas:  2,
		Placement: &nats.Placement{Cluster: "C2"},
		Sources:   sourceForCluster("C3"),
		MaxAge:    time.Minute,
		MaxMsgs:   20_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "C3-S-MUX",
		Replicas:  2,
		Placement: &nats.Placement{Cluster: "C3"},
		Sources:   sourceForCluster("C1"),
		MaxAge:    time.Minute,
		MaxMsgs:   20_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Now create mirrors for our mux'd streams.
	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "C1-MIRROR",
		Replicas:  3,
		Placement: &nats.Placement{Cluster: "C1"},
		Mirror:    &nats.StreamSource{Name: "C3-S-MUX"},
		MaxAge:    5 * time.Minute,
		MaxMsgs:   10_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "C2-MIRROR",
		Replicas:  3,
		Placement: &nats.Placement{Cluster: "C2"},
		Mirror:    &nats.StreamSource{Name: "C2-S-MUX"},
		MaxAge:    5 * time.Minute,
		MaxMsgs:   10_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:      "C3-MIRROR",
		Replicas:  3,
		Placement: &nats.Placement{Cluster: "C3"},
		Mirror:    &nats.StreamSource{Name: "C1-S-MUX"},
		MaxAge:    5 * time.Minute,
		MaxMsgs:   10_000,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	var jsc []nats.JetStream

	// Create 64 clients.
	for i := 0; i < 64; i++ {
		s := sc.randomCluster().randomServer()
		nc, _ := jsClientConnect(t, s)
		defer nc.Close()
		js, err := nc.JetStream(nats.PublishAsyncMaxPending(8 * 1024))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		jsc = append(jsc, js)
	}

	msg := make([]byte, 1024)
	rand.Read(msg)

	// 10 minutes
	expires := time.Now().Add(480 * time.Second)
	for time.Now().Before(expires) {
		for _, sns := range scm {
			rand.Shuffle(len(sns), func(i, j int) { sns[i], sns[j] = sns[j], sns[i] })
			for _, sn := range sns {
				js := jsc[rand.Intn(len(jsc))]
				if _, err = js.PublishAsync(sn, msg); err != nil {
					t.Fatalf("Unexpected publish error: %v", err)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoRaceJetStreamSlowFilteredInititalPendingAndFirstMsg(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	// Create directly here to force multiple blocks, etc.
	a, err := s.LookupAccount("$G")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	mset, err := a.addStreamWithStore(
		&StreamConfig{
			Name:     "S",
			Subjects: []string{"foo", "bar", "baz", "foo.bar.baz", "foo.*"},
		},
		&FileStoreConfig{
			BlockSize:  4 * 1024 * 1024,
			AsyncFlush: true,
		},
	)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	toSend := 100_000 // 500k total though.

	// Messages will be 'foo' 'bar' 'baz' repeated 100k times.
	// Then 'foo.bar.baz' all contigous for 100k.
	// Then foo.N for 1-100000
	for i := 0; i < toSend; i++ {
		js.PublishAsync("foo", []byte("HELLO"))
		js.PublishAsync("bar", []byte("WORLD"))
		js.PublishAsync("baz", []byte("AGAIN"))
	}
	// Make contiguous block of same subject.
	for i := 0; i < toSend; i++ {
		js.PublishAsync("foo.bar.baz", []byte("ALL-TOGETHER"))
	}
	// Now add some more at the end.
	for i := 0; i < toSend; i++ {
		js.PublishAsync(fmt.Sprintf("foo.%d", i+1), []byte("LATER"))
	}

	checkFor(t, 10*time.Second, 250*time.Millisecond, func() error {
		si, err := js.StreamInfo("S")
		if err != nil {
			return err
		}
		if si.State.Msgs != uint64(5*toSend) {
			return fmt.Errorf("Expected %d msgs, got %d", 5*toSend, si.State.Msgs)
		}
		return nil
	})

	// Threshold for taking too long.
	const thresh = 50 * time.Millisecond

	var dindex int
	testConsumerCreate := func(subj string, startSeq, expectedNumPending uint64) {
		t.Helper()
		dindex++
		dname := fmt.Sprintf("dur-%d", dindex)
		cfg := ConsumerConfig{FilterSubject: subj, Durable: dname, AckPolicy: AckExplicit}
		if startSeq > 1 {
			cfg.OptStartSeq, cfg.DeliverPolicy = startSeq, DeliverByStartSequence
		}
		start := time.Now()
		o, err := mset.addConsumer(&cfg)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if delta := time.Since(start); delta > thresh {
			t.Fatalf("Creating consumer for %q and start: %d took too long: %v", subj, startSeq, delta)
		}
		if ci := o.info(); ci.NumPending != expectedNumPending {
			t.Fatalf("Expected NumPending of %d, got %d", expectedNumPending, ci.NumPending)
		}
	}

	testConsumerCreate("foo.100000", 1, 1)
	testConsumerCreate("foo.100000", 222_000, 1)
	testConsumerCreate("foo", 1, 100_000)
	testConsumerCreate("foo", 4, 100_000-1)
	testConsumerCreate("foo.bar.baz", 1, 100_000)
	testConsumerCreate("foo.bar.baz", 350_001, 50_000)
	testConsumerCreate("*", 1, 300_000)
	testConsumerCreate("*", 4, 300_000-3)
	testConsumerCreate(">", 1, 500_000)
	testConsumerCreate(">", 50_000, 500_000-50_000+1)
	testConsumerCreate("foo.10", 1, 1)

	// Also test that we do not take long if the start sequence is later in the stream.
	sub, err := js.PullSubscribe("foo.100000", "dlc")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	start := time.Now()
	fetchMsgs(t, sub, 1, time.Second)
	if delta := time.Since(start); delta > thresh {
		t.Fatalf("Took too long for pull subscriber to fetch the message: %v", delta)
	}

	// Now do some deletes and make sure these are handled correctly.
	// Delete 3 foo messages.
	mset.removeMsg(1)
	mset.removeMsg(4)
	mset.removeMsg(7)
	testConsumerCreate("foo", 1, 100_000-3)

	// Make sure wider scoped subjects do the right thing from a pending perspective.
	o, err := mset.addConsumer(&ConsumerConfig{FilterSubject: ">", Durable: "cat", AckPolicy: AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	ci, expected := o.info(), uint64(500_000-3)
	if ci.NumPending != expected {
		t.Fatalf("Expected NumPending of %d, got %d", expected, ci.NumPending)
	}
	// Send another and make sure its captured by our wide scope consumer.
	js.Publish("foo", []byte("HELLO AGAIN"))
	if ci = o.info(); ci.NumPending != expected+1 {
		t.Fatalf("Expected the consumer to recognize the wide scoped consumer, wanted pending of %d, got %d", expected+1, ci.NumPending)
	}

	// Stop current server and test restart..
	sd := s.JetStreamConfig().StoreDir
	s.Shutdown()
	// Restart.
	s = RunJetStreamServerOnPort(-1, sd)
	defer s.Shutdown()

	a, err = s.LookupAccount("$G")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	mset, err = a.lookupStream("S")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Make sure we recovered our per subject state on restart.
	testConsumerCreate("foo.100000", 1, 1)
	testConsumerCreate("foo", 1, 100_000-2)
}

func TestNoRaceJetStreamFileStoreBufferReuse(t *testing.T) {
	// Uncomment to run. Needs to be on a big machine.
	skip(t)

	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	cfg := &StreamConfig{Name: "TEST", Subjects: []string{"foo", "bar", "baz"}, Storage: FileStorage}
	if _, err := s.GlobalAccount().addStreamWithStore(cfg, nil); err != nil {
		t.Fatalf("Unexpected error adding stream: %v", err)
	}

	// Client for API requests.
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	toSend := 200_000

	m := nats.NewMsg("foo")
	m.Data = make([]byte, 8*1024)
	rand.Read(m.Data)

	start := time.Now()
	for i := 0; i < toSend; i++ {
		m.Reply = _EMPTY_
		switch i % 3 {
		case 0:
			m.Subject = "foo"
		case 1:
			m.Subject = "bar"
		case 2:
			m.Subject = "baz"
		}
		m.Header.Set("X-ID2", fmt.Sprintf("XXXXX-%d", i))
		if _, err := js.PublishMsgAsync(m); err != nil {
			t.Fatalf("Err on publish: %v", err)
		}
	}
	<-js.PublishAsyncComplete()
	fmt.Printf("TOOK %v to publish\n", time.Since(start))

	v, err := s.Varz(nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	fmt.Printf("MEM AFTER PUBLISH is %v\n", friendlyBytes(v.Mem))

	si, _ := js.StreamInfo("TEST")
	fmt.Printf("si is %+v\n", si.State)

	received := 0
	done := make(chan bool)

	cb := func(m *nats.Msg) {
		received++
		if received >= toSend {
			done <- true
		}
	}

	start = time.Now()
	sub, err := js.Subscribe("*", cb, nats.EnableFlowControl(), nats.IdleHeartbeat(time.Second), nats.AckNone())
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer sub.Unsubscribe()
	<-done
	fmt.Printf("TOOK %v to consume\n", time.Since(start))

	v, err = s.Varz(nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	fmt.Printf("MEM AFTER SUBSCRIBE is %v\n", friendlyBytes(v.Mem))
}

// Report of slow restart for a server that has many messages that have expired while it was not running.
func TestNoRaceJetStreamSlowRestartWithManyExpiredMsgs(t *testing.T) {
	opts := DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	s := RunServer(&opts)
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	// Client for API requests.
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	ttl := 2 * time.Second
	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "ORDERS",
		Subjects: []string{"orders.*"},
		MaxAge:   ttl,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Attach a consumer who is filtering on a wildcard subject as well.
	// This does not affect it like I thought originally but will keep it here.
	_, err = js.AddConsumer("ORDERS", &nats.ConsumerConfig{
		Durable:       "c22",
		FilterSubject: "orders.*",
		AckPolicy:     nats.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Now fill up with messages.
	toSend := 100_000
	for i := 1; i <= toSend; i++ {
		js.PublishAsync(fmt.Sprintf("orders.%d", i), []byte("OK"))
	}
	<-js.PublishAsyncComplete()

	sdir := strings.TrimSuffix(s.JetStreamConfig().StoreDir, JetStreamStoreDir)
	s.Shutdown()

	// Let them expire while not running.
	time.Sleep(ttl + 500*time.Millisecond)

	start := time.Now()
	opts.Port = -1
	opts.StoreDir = sdir
	s = RunServer(&opts)
	elapsed := time.Since(start)
	defer s.Shutdown()

	if elapsed > 2*time.Second {
		t.Fatalf("Took %v for restart which is too long", elapsed)
	}

	// Check everything is correct.
	nc, js = jsClientConnect(t, s)
	defer nc.Close()

	si, err := js.StreamInfo("ORDERS")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if si.State.Msgs != 0 {
		t.Fatalf("Expected no msgs after restart, got %d", si.State.Msgs)
	}
}

func TestNoRaceJetStreamStalledMirrorsAfterExpire(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	cfg := &nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo.*"},
		Replicas: 1,
		MaxAge:   100 * time.Millisecond,
	}

	if _, err := js.AddStream(cfg); err != nil {
		t.Fatalf("Error creating stream: %v", err)
	}

	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     "M",
		Replicas: 2,
		Mirror:   &nats.StreamSource{Name: "TEST"},
	}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	sendBatch := func(batch int) {
		t.Helper()
		for i := 0; i < batch; i++ {
			js.PublishAsync("foo.bar", []byte("Hello"))
		}
		select {
		case <-js.PublishAsyncComplete():
		case <-time.After(5 * time.Second):
			t.Fatalf("Did not receive completion signal")
		}
	}

	numMsgs := 10_000
	sendBatch(numMsgs)

	// Turn off expiration so we can test we did not stall.
	cfg.MaxAge = 0
	if _, err := js.UpdateStream(cfg); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	sendBatch(numMsgs)

	// Wait for mirror to be caught up.
	checkFor(t, 10*time.Second, 500*time.Millisecond, func() error {
		si, err := js.StreamInfo("M")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.LastSeq != uint64(2*numMsgs) {
			return fmt.Errorf("Expected %d as last sequence, got state: %+v", 2*numMsgs, si.State)
		}
		return nil
	})
}

// We will use JetStream helpers to create supercluster but this test is about exposing the ability to access
// account scoped connz with subject interest filtering.
func TestNoRaceJetStreamSuperClusterAccountConnz(t *testing.T) {
	// This has 4 different account, 3 general and system.
	sc := createJetStreamSuperClusterWithTemplate(t, jsClusterAccountsTempl, 3, 3)
	defer sc.shutdown()

	// Create 20 connections on account one and two
	// Create JetStream assets for each as well to make sure by default we do not report them.
	num := 20
	for i := 0; i < num; i++ {
		nc, _ := jsClientConnect(t, sc.randomServer(), nats.UserInfo("one", "p"), nats.Name("one"))
		defer nc.Close()

		if i%2 == 0 {
			nc.SubscribeSync("foo")
		} else {
			nc.SubscribeSync("bar")
		}

		nc, js := jsClientConnect(t, sc.randomServer(), nats.UserInfo("two", "p"), nats.Name("two"))
		nc.SubscribeSync("baz")
		nc.SubscribeSync("foo.bar.*")
		nc.SubscribeSync(fmt.Sprintf("id.%d", i+1))

		js.AddStream(&nats.StreamConfig{Name: fmt.Sprintf("TEST:%d", i+1)})

		defer nc.Close()
	}

	type czapi struct {
		Server *ServerInfo
		Data   *Connz
		Error  *ApiError
	}

	parseConnz := func(buf []byte) *Connz {
		t.Helper()
		var cz czapi
		if err := json.Unmarshal(buf, &cz); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if cz.Error != nil {
			t.Fatalf("Unexpected error: %+v", cz.Error)
		}
		return cz.Data
	}

	doRequest := func(reqSubj, acc, filter string, expected int) {
		t.Helper()
		nc, _ := jsClientConnect(t, sc.randomServer(), nats.UserInfo(acc, "p"), nats.Name(acc))
		defer nc.Close()

		mch := make(chan *nats.Msg, 9)
		sub, _ := nc.ChanSubscribe(nats.NewInbox(), mch)

		var req []byte
		if filter != _EMPTY_ {
			req, _ = json.Marshal(&ConnzOptions{FilterSubject: filter})
		}

		if err := nc.PublishRequest(reqSubj, sub.Subject, req); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// So we can igniore ourtselves.
		cid, _ := nc.GetClientID()
		sid := nc.ConnectedServerId()

		wt := time.NewTimer(200 * time.Millisecond)
		var conns []*ConnInfo
	LOOP:
		for {
			select {
			case m := <-mch:
				if len(m.Data) == 0 {
					t.Fatalf("No responders")
				}
				cr := parseConnz(m.Data)
				// For account scoped, NumConns and Total should be the same (sans limits and offsets).
				// It Total should not include other accounts since that would leak information about the system.
				if filter == _EMPTY_ && cr.NumConns != cr.Total {
					t.Fatalf("NumConns and Total should be same with account scoped connz, got %+v", cr)
				}
				for _, c := range cr.Conns {
					if c.Name != acc {
						t.Fatalf("Got wrong account: %q vs %q for %+v", acc, c.Account, c)
					}
					if !(c.Cid == cid && cr.ID == sid) {
						conns = append(conns, c)
					}
				}
				wt.Reset(200 * time.Millisecond)
			case <-wt.C:
				break LOOP
			}
		}
		if len(conns) != expected {
			t.Fatalf("Expected to see %d conns but got %d", expected, len(conns))
		}
	}

	doSysRequest := func(acc string, expected int) {
		t.Helper()
		doRequest("$SYS.REQ.SERVER.PING.CONNZ", acc, _EMPTY_, expected)
	}
	doAccRequest := func(acc string, expected int) {
		t.Helper()
		doRequest("$SYS.REQ.ACCOUNT.PING.CONNZ", acc, _EMPTY_, expected)
	}
	doFiltered := func(acc, filter string, expected int) {
		t.Helper()
		doRequest("$SYS.REQ.SERVER.PING.CONNZ", acc, filter, expected)
	}

	doSysRequest("one", 20)
	doAccRequest("one", 20)

	doSysRequest("two", 20)
	doAccRequest("two", 20)

	// Now check filtering.
	doFiltered("one", _EMPTY_, 20)
	doFiltered("one", ">", 20)
	doFiltered("one", "bar", 10)
	doFiltered("two", "bar", 0)
	doFiltered("two", "id.1", 1)
	doFiltered("two", "id.*", 20)
	doFiltered("two", "foo.bar.*", 20)
	doFiltered("two", "foo.>", 20)
}

func TestNoRaceCompressedConnz(t *testing.T) {
	s := RunBasicJetStreamServer()
	config := s.JetStreamConfig()
	if config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, _ := jsClientConnect(t, s)
	defer nc.Close()

	doRequest := func(compress string) {
		t.Helper()
		m := nats.NewMsg("$SYS.REQ.ACCOUNT.PING.CONNZ")
		m.Header.Add("Accept-Encoding", compress)
		resp, err := nc.RequestMsg(m, time.Second)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		buf := resp.Data

		// Make sure we have an encoding header.
		ce := resp.Header.Get("Content-Encoding")
		switch strings.ToLower(ce) {
		case "gzip":
			zr, err := gzip.NewReader(bytes.NewReader(buf))
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			defer zr.Close()
			buf, err = ioutil.ReadAll(zr)
			if err != nil && err != io.ErrUnexpectedEOF {
				t.Fatalf("Unexpected error: %v", err)
			}
		case "snappy", "s2":
			sr := s2.NewReader(bytes.NewReader(buf))
			buf, err = ioutil.ReadAll(sr)
			if err != nil && err != io.ErrUnexpectedEOF {
				t.Fatalf("Unexpected error: %v", err)
			}
		default:
			t.Fatalf("Unknown content-encoding of %q", ce)
		}

		var cz ServerAPIConnzResponse
		if err := json.Unmarshal(buf, &cz); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if cz.Error != nil {
			t.Fatalf("Unexpected error: %+v", cz.Error)
		}
	}

	doRequest("gzip")
	doRequest("snappy")
	doRequest("s2")
}

func TestNoRaceJetStreamClusterExtendedStreamPurge(t *testing.T) {
	for _, st := range []StorageType{FileStorage, MemoryStorage} {
		t.Run(st.String(), func(t *testing.T) {
			c := createJetStreamClusterExplicit(t, "JSC", 3)
			defer c.shutdown()

			nc, js := jsClientConnect(t, c.randomServer())
			defer nc.Close()

			cfg := StreamConfig{
				Name:       "KV",
				Subjects:   []string{"kv.>"},
				Storage:    st,
				Replicas:   2,
				MaxMsgsPer: 100,
			}
			req, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			// Do manually for now.
			nc.Request(fmt.Sprintf(JSApiStreamCreateT, cfg.Name), req, time.Second)
			c.waitOnStreamLeader("$G", "KV")

			si, err := js.StreamInfo("KV")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if si == nil || si.Config.Name != "KV" {
				t.Fatalf("StreamInfo is not correct %+v", si)
			}

			for i := 0; i < 1000; i++ {
				js.PublishAsync("kv.foo", []byte("OK")) // 1 * i
				js.PublishAsync("kv.bar", []byte("OK")) // 2 * i
				js.PublishAsync("kv.baz", []byte("OK")) // 3 * i
			}
			// First is 2700, last is 3000
			for i := 0; i < 700; i++ {
				js.PublishAsync(fmt.Sprintf("kv.%d", i+1), []byte("OK"))
			}
			// Now first is 2700, last is 3700
			select {
			case <-js.PublishAsyncComplete():
			case <-time.After(10 * time.Second):
				t.Fatalf("Did not receive completion signal")
			}

			si, err = js.StreamInfo("KV")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if si.State.Msgs != 1000 {
				t.Fatalf("Expected %d msgs, got %d", 1000, si.State.Msgs)
			}

			shouldFail := func(preq *JSApiStreamPurgeRequest) {
				req, _ := json.Marshal(preq)
				resp, err := nc.Request(fmt.Sprintf(JSApiStreamPurgeT, "KV"), req, time.Second)
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				var pResp JSApiStreamPurgeResponse
				if err = json.Unmarshal(resp.Data, &pResp); err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if pResp.Success || pResp.Error == nil {
					t.Fatalf("Expected an error response but got none")
				}
			}

			// Sequence and Keep should be mutually exclusive.
			shouldFail(&JSApiStreamPurgeRequest{Sequence: 10, Keep: 10})

			purge := func(preq *JSApiStreamPurgeRequest, newTotal uint64) {
				t.Helper()
				req, _ := json.Marshal(preq)
				resp, err := nc.Request(fmt.Sprintf(JSApiStreamPurgeT, "KV"), req, time.Second)
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				var pResp JSApiStreamPurgeResponse
				if err = json.Unmarshal(resp.Data, &pResp); err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if !pResp.Success || pResp.Error != nil {
					t.Fatalf("Got a bad response %+v", pResp)
				}
				si, err = js.StreamInfo("KV")
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if si.State.Msgs != newTotal {
					t.Fatalf("Expected total after purge to be %d but got %d", newTotal, si.State.Msgs)
				}
			}
			expectLeft := func(subject string, expected uint64) {
				t.Helper()
				ci, err := js.AddConsumer("KV", &nats.ConsumerConfig{Durable: "dlc", FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy})
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				defer js.DeleteConsumer("KV", "dlc")
				if ci.NumPending != expected {
					t.Fatalf("Expected %d remaining but got %d", expected, ci.NumPending)
				}
			}

			purge(&JSApiStreamPurgeRequest{Subject: "kv.foo"}, 900)
			expectLeft("kv.foo", 0)

			purge(&JSApiStreamPurgeRequest{Subject: "kv.bar", Keep: 1}, 801)
			expectLeft("kv.bar", 1)

			purge(&JSApiStreamPurgeRequest{Subject: "kv.baz", Sequence: 2851}, 751)
			expectLeft("kv.baz", 50)

			purge(&JSApiStreamPurgeRequest{Subject: "kv.*"}, 0)

			// RESET
			js.DeleteStream("KV")
			// Do manually for now.
			nc.Request(fmt.Sprintf(JSApiStreamCreateT, cfg.Name), req, time.Second)
			c.waitOnStreamLeader("$G", "KV")

			if _, err := js.StreamInfo("KV"); err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			// Put in 100.
			for i := 0; i < 100; i++ {
				js.PublishAsync("kv.foo", []byte("OK"))
			}
			select {
			case <-js.PublishAsyncComplete():
			case <-time.After(time.Second):
				t.Fatalf("Did not receive completion signal")
			}
			purge(&JSApiStreamPurgeRequest{Subject: "kv.foo", Keep: 10}, 10)
			purge(&JSApiStreamPurgeRequest{Subject: "kv.foo", Keep: 10}, 10)
			expectLeft("kv.foo", 10)

			// RESET AGAIN
			js.DeleteStream("KV")
			// Do manually for now.
			nc.Request(fmt.Sprintf(JSApiStreamCreateT, cfg.Name), req, time.Second)
			c.waitOnStreamLeader("$G", "KV")

			if _, err := js.StreamInfo("KV"); err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			// Put in 100.
			for i := 0; i < 100; i++ {
				js.Publish("kv.foo", []byte("OK"))
			}
			purge(&JSApiStreamPurgeRequest{Keep: 10}, 10)
			expectLeft(">", 10)

			// RESET AGAIN
			js.DeleteStream("KV")
			// Do manually for now.
			nc.Request(fmt.Sprintf(JSApiStreamCreateT, cfg.Name), req, time.Second)
			if _, err := js.StreamInfo("KV"); err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			// Put in 100.
			for i := 0; i < 100; i++ {
				js.Publish("kv.foo", []byte("OK"))
			}
			purge(&JSApiStreamPurgeRequest{Sequence: 90}, 11) // Up to 90 so we keep that, hence the 11.
			expectLeft(">", 11)
		})
	}
}

func TestNoRaceJetStreamFileStoreCompaction(t *testing.T) {
	s := RunBasicJetStreamServer()
	config := s.JetStreamConfig()
	if config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	cfg := &nats.StreamConfig{
		Name:              "KV",
		Subjects:          []string{"KV.>"},
		MaxMsgsPerSubject: 1,
	}
	if _, err := js.AddStream(cfg); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	toSend := 10_000
	data := make([]byte, 4*1024)
	rand.Read(data)

	// First one.
	js.PublishAsync("KV.FM", data)

	for i := 0; i < toSend; i++ {
		js.PublishAsync(fmt.Sprintf("KV.%d", i+1), data)
	}
	// Do again and overwrite the previous batch.
	for i := 0; i < toSend; i++ {
		js.PublishAsync(fmt.Sprintf("KV.%d", i+1), data)
	}

	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	// Now check by hand the utilization level.
	mset, err := s.GlobalAccount().lookupStream("KV")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	total, used, _ := mset.Store().Utilization()
	if pu := 100.0 * float32(used) / float32(total); pu < 80.0 {
		t.Fatalf("Utilization is less than 80%%, got %.2f", pu)
	}
}

func TestNoRaceJetStreamEncryptionEnabledOnRestartWithExpire(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		jetstream: enabled
	`))
	defer removeFile(t, conf)

	s, _ := RunServerWithConfig(conf)
	defer s.Shutdown()

	config := s.JetStreamConfig()
	if config == nil {
		t.Fatalf("Expected config but got none")
	}
	defer removeDir(t, config.StoreDir)

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	toSend := 10_000

	cfg := &nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		MaxMsgs:  int64(toSend),
	}
	if _, err := js.AddStream(cfg); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	data := make([]byte, 4*1024) // 4K payload
	rand.Read(data)

	for i := 0; i < toSend; i++ {
		js.PublishAsync("foo", data)
		js.PublishAsync("bar", data)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	_, err := js.AddConsumer("TEST", &nats.ConsumerConfig{Durable: "dlc", AckPolicy: nats.AckExplicitPolicy})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Restart
	nc.Close()
	s.Shutdown()

	ncs := fmt.Sprintf("\nlisten: 127.0.0.1:-1\njetstream: {key: %q, store_dir: %q}\n", "s3cr3t!", config.StoreDir)
	conf = createConfFile(t, []byte(ncs))
	defer removeFile(t, conf)

	// Try to drain entropy to see if effects startup time.
	drain := make([]byte, 32*1024*1024) // Pull 32Mb of crypto rand.
	crand.Read(drain)

	start := time.Now()
	s, _ = RunServerWithConfig(conf)
	defer s.Shutdown()
	dd := time.Since(start)
	if dd > 5*time.Second {
		t.Fatalf("Restart took longer than expected: %v", dd)
	}
}

// This test was from Ivan K. and showed a bug in the filestore implementation.
// This is skipped by default since it takes >40s to run.
func TestNoRaceJetStreamOrderedConsumerMissingMsg(t *testing.T) {
	// Uncomment to run. Needs to be on a big machine. Do not want as part of Travis tests atm.
	skip(t)

	s := RunBasicJetStreamServer()
	config := s.JetStreamConfig()
	if config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     "benchstream",
		Subjects: []string{"testsubject"},
		Replicas: 1,
	}); err != nil {
		t.Fatalf("add stream failed: %s", err)
	}

	total := 1_000_000

	numSubs := 10
	ch := make(chan struct{}, numSubs)
	wg := sync.WaitGroup{}
	wg.Add(numSubs)
	errCh := make(chan error, 1)
	for i := 0; i < numSubs; i++ {
		nc, js := jsClientConnect(t, s)
		defer nc.Close()
		go func(nc *nats.Conn, js nats.JetStreamContext) {
			defer wg.Done()
			received := 0
			_, err := js.Subscribe("testsubject", func(m *nats.Msg) {
				meta, _ := m.Metadata()
				if meta.Sequence.Consumer != meta.Sequence.Stream {
					nc.Close()
					errCh <- fmt.Errorf("Bad meta: %+v", meta)
				}
				received++
				if received == total {
					ch <- struct{}{}
				}
			}, nats.OrderedConsumer())
			if err != nil {
				select {
				case errCh <- fmt.Errorf("Error creating sub: %v", err):
				default:
				}

			}
		}(nc, js)
	}
	wg.Wait()
	select {
	case e := <-errCh:
		t.Fatal(e)
	default:
	}

	payload := make([]byte, 500)
	for i := 1; i <= total; i++ {
		js.PublishAsync("testsubject", payload)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(10 * time.Second):
		t.Fatalf("Did not send all messages")
	}

	// Now wait for consumers to be done:
	for i := 0; i < numSubs; i++ {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("Did not receive all messages for all consumers in time")
		}
	}

}

// Issue #2488 - Bad accounting, can not reproduce the stalled consumers after last several PRs.
// Issue did show bug in ack logic for no-ack and interest based retention.
func TestNoRaceJetStreamClusterInterestPolicyAckNone(t *testing.T) {
	for _, test := range []struct {
		name    string
		durable string
	}{
		{"durable", "dlc"},
		{"ephemeral", _EMPTY_},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := createJetStreamClusterExplicit(t, "R3S", 3)
			defer c.shutdown()

			// Client based API
			nc, js := jsClientConnect(t, c.randomServer())
			defer nc.Close()

			_, err := js.AddStream(&nats.StreamConfig{
				Name:      "cluster",
				Subjects:  []string{"cluster.*"},
				Retention: nats.InterestPolicy,
				Discard:   nats.DiscardOld,
				Replicas:  3,
			})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			var received uint32
			mh := func(m *nats.Msg) {
				atomic.AddUint32(&received, 1)
			}

			_, err = js.Subscribe("cluster.created", mh, nats.Durable(test.durable), nats.DeliverNew(), nats.AckNone())
			if err != nil {
				t.Fatalf("Unexepected error: %v", err)
			}

			msg := []byte("ACK ME")
			const total = uint32(1_000)
			for i := 0; i < int(total); i++ {
				if _, err := js.Publish("cluster.created", msg); err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				//time.Sleep(100 * time.Microsecond)
			}

			// Wait for all messages to be received.
			checkFor(t, 2*time.Second, 100*time.Millisecond, func() error {
				r := atomic.LoadUint32(&received)
				if r == total {
					return nil
				}
				return fmt.Errorf("Received only %d out of %d", r, total)
			})

			checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
				si, err := js.StreamInfo("cluster")
				if err != nil {
					t.Fatalf("Error getting stream info: %v", err)
				}
				if si.State.Msgs != 0 {
					return fmt.Errorf("Expected no messages, got %d", si.State.Msgs)
				}
				return nil
			})
		})
	}
}

// There was a bug in the filestore compact code that would cause a store
// with JSExpectedLastSubjSeq to fail with "wrong last sequence: 0"
func TestNoRaceJetStreamLastSubjSeqAndFilestoreCompact(t *testing.T) {
	s := RunBasicJetStreamServer()
	defer s.Shutdown()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:              "MQTT_sess",
		Subjects:          []string{"MQTT.sess.>"},
		Storage:           nats.FileStorage,
		Retention:         nats.LimitsPolicy,
		Replicas:          1,
		MaxMsgsPerSubject: 1,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	firstPayload := make([]byte, 40)
	secondPayload := make([]byte, 380)
	for iter := 0; iter < 2; iter++ {
		for i := 0; i < 4000; i++ {
			subj := "MQTT.sess." + string(getHash(fmt.Sprintf("client_%d", i)))
			pa, err := js.Publish(subj, firstPayload)
			if err != nil {
				t.Fatalf("Error on publish: %v", err)
			}
			m := nats.NewMsg(subj)
			m.Data = secondPayload
			eseq := strconv.FormatInt(int64(pa.Sequence), 10)
			m.Header.Set(JSExpectedLastSubjSeq, eseq)
			if _, err := js.PublishMsg(m); err != nil {
				t.Fatalf("Error on publish (iter=%v seq=%v): %v", iter+1, pa.Sequence, err)
			}
		}
	}
}

// Issue #2548
func TestNoRaceJetStreamClusterMemoryStreamConsumerRaftGrowth(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:      "memory-leak",
		Subjects:  []string{"memory-leak"},
		Retention: nats.LimitsPolicy,
		MaxMsgs:   1000,
		Discard:   nats.DiscardOld,
		MaxAge:    time.Minute,
		Storage:   nats.MemoryStorage,
		Replicas:  3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	_, err = js.QueueSubscribe("memory-leak", "q1", func(msg *nats.Msg) {
		time.Sleep(1 * time.Second)
		msg.AckSync()
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Send 10k (Must be > 8192 which is compactNumMin from monitorConsumer.
	msg := []byte("NATS is a connective technology that powers modern distributed systems.")
	for i := 0; i < 10_000; i++ {
		if _, err := js.Publish("memory-leak", msg); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	// We will verify here that the underlying raft layer for the leader is not > 8192
	cl := c.consumerLeader("$G", "memory-leak", "q1")
	mset, err := cl.GlobalAccount().lookupStream("memory-leak")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	o := mset.lookupConsumer("q1")
	if o == nil {
		t.Fatalf("Error looking up consumer %q", "q1")
	}
	node := o.raftNode().(*raft)
	checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
		if ms := node.wal.(*memStore); ms.State().Msgs > 8192 {
			return fmt.Errorf("Did not compact the raft memory WAL")
		}
		return nil
	})
}

func TestNoRaceJetStreamClusterCorruptWAL(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	if _, err := js.AddStream(&nats.StreamConfig{Name: "TEST", Subjects: []string{"foo"}, Replicas: 3}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	sub, err := js.PullSubscribe("foo", "dlc")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	numMsgs := 1000
	for i := 0; i < numMsgs; i++ {
		js.PublishAsync("foo", []byte("WAL"))
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	for i, m := range fetchMsgs(t, sub, 200, 5*time.Second) {
		// Ack first 50 and every other even on after that..
		if i < 50 || i%2 == 1 {
			m.AckSync()
		}
	}
	// Make sure acks processed.
	time.Sleep(200 * time.Millisecond)
	nc.Close()

	// Check consumer consistency.
	checkConsumerWith := func(delivered, ackFloor uint64, ackPending int) {
		t.Helper()
		nc, js := jsClientConnect(t, c.randomServer())
		defer nc.Close()

		checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
			ci, err := js.ConsumerInfo("TEST", "dlc")
			if err != nil {
				return fmt.Errorf("Unexpected error: %v", err)
			}
			if ci.Delivered.Consumer != ci.Delivered.Stream || ci.Delivered.Consumer != delivered {
				return fmt.Errorf("Expected %d for delivered, got %+v", delivered, ci.Delivered)
			}
			if ci.AckFloor.Consumer != ci.AckFloor.Stream || ci.AckFloor.Consumer != ackFloor {
				return fmt.Errorf("Expected %d for ack floor, got %+v", ackFloor, ci.AckFloor)
			}
			nm := uint64(numMsgs)
			if ci.NumPending != nm-delivered {
				return fmt.Errorf("Expected num pending to be %d, got %d", nm-delivered, ci.NumPending)
			}
			if ci.NumAckPending != ackPending {
				return fmt.Errorf("Expected num ack pending to be %d, got %d", ackPending, ci.NumAckPending)
			}
			return nil
		})
	}

	checkConsumer := func() {
		t.Helper()
		checkConsumerWith(200, 50, 75)
	}

	checkConsumer()

	// Grab the consumer leader.
	cl := c.consumerLeader("$G", "TEST", "dlc")
	mset, err := cl.GlobalAccount().lookupStream("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	o := mset.lookupConsumer("dlc")
	if o == nil {
		t.Fatalf("Error looking up consumer %q", "dlc")
	}
	// Grab underlying raft node and the WAL (filestore) and we will attempt to "corrupt" it.
	node := o.raftNode().(*raft)
	fs := node.wal.(*fileStore)
	fcfg, cfg := fs.fcfg, fs.cfg.StreamConfig
	// Stop all the servers.
	c.stopAll()

	// Manipulate directly with cluster down.
	fs, err = newFileStore(fcfg, cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	state := fs.State()
	sm, err := fs.LoadMsg(state.LastSeq, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	ae, err := node.decodeAppendEntry(sm.msg, nil, _EMPTY_)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	dentry := func(dseq, sseq, dc uint64, ts int64) []byte {
		b := make([]byte, 4*binary.MaxVarintLen64+1)
		b[0] = byte(updateDeliveredOp)
		n := 1
		n += binary.PutUvarint(b[n:], dseq)
		n += binary.PutUvarint(b[n:], sseq)
		n += binary.PutUvarint(b[n:], dc)
		n += binary.PutVarint(b[n:], ts)
		return b[:n]
	}

	// Let's put a non-contigous AppendEntry into the system.
	ae.pindex += 10
	// Add in delivered record.
	ae.entries = []*Entry{&Entry{EntryNormal, dentry(1000, 1000, 1, time.Now().UnixNano())}}
	encoded, err := ae.encode(nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if _, _, err := fs.StoreMsg(_EMPTY_, nil, encoded); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	fs.Stop()

	c.restartAllSamePorts()
	c.waitOnStreamLeader("$G", "TEST")
	c.waitOnConsumerLeader("$G", "TEST", "dlc")

	checkConsumer()

	// Now we will truncate out the WAL out from underneath the leader.
	// Grab the consumer leader.

	nc, js = jsClientConnect(t, c.randomServer())
	defer nc.Close()

	cl = c.consumerLeader("$G", "TEST", "dlc")
	mset, err = cl.GlobalAccount().lookupStream("TEST")
	require_NoError(t, err)
	o = mset.lookupConsumer("dlc")
	require_NoError(t, err)

	// Grab underlying raft node and the WAL (filestore) and truncate it.
	// This will simulate the WAL losing state due to truncate and we want to make sure it recovers.

	fs = o.raftNode().(*raft).wal.(*fileStore)
	state = fs.State()
	err = fs.Truncate(state.FirstSeq)
	require_NoError(t, err)
	state = fs.State()

	sub, err = js.PullSubscribe("foo", "dlc")
	require_NoError(t, err)

	// This will cause us to stepdown and truncate our WAL.
	sub.Fetch(100)
	c.waitOnConsumerLeader("$G", "TEST", "dlc")
	// We can't trust the results sans that we have a leader back in place and the ackFloor.
	ci, err := js.ConsumerInfo("TEST", "dlc")
	require_NoError(t, err)
	if ci.AckFloor.Consumer != ci.AckFloor.Stream || ci.AckFloor.Consumer != 50 {
		t.Fatalf("Expected %d for ack floor, got %+v", 50, ci.AckFloor)
	}
}

func TestNoRaceJetStreamClusterInterestRetentionDeadlock(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	s := c.randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	// This can trigger deadlock with current architecture.
	// If stream is !limitsRetention and consumer is DIRECT and ack none we will try to place the msg seq
	// onto a chan for the stream to consider removing. All conditions above must hold to trigger.

	// We will attempt to trigger here with a stream mirror setup which uses and R=1 DIRECT consumer to replicate msgs.
	_, err := js.AddStream(&nats.StreamConfig{Name: "S", Retention: nats.InterestPolicy, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Create a mirror which will create the consumer profile to trigger.
	_, err = js.AddStream(&nats.StreamConfig{Name: "M", Mirror: &nats.StreamSource{Name: "S"}})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Queue up alot of messages.
	numRequests := 20_000
	for i := 0; i < numRequests; i++ {
		js.PublishAsync("S", []byte("Q"))
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
		si, err := js.StreamInfo("S")
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if si.State.Msgs != 0 {
			return fmt.Errorf("Expected 0 msgs, got state: %+v", si.State)
		}
		return nil
	})
}

func TestNoRaceJetStreamClusterMaxConsumersAndDirect(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	s := c.randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	// We want to max sure max consumer limits do not affect mirrors or sources etc.
	_, err := js.AddStream(&nats.StreamConfig{Name: "S", Storage: nats.MemoryStorage, MaxConsumers: 1})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	var mirrors []string
	for i := 0; i < 10; i++ {
		// Create a mirror.
		mname := fmt.Sprintf("M-%d", i+1)
		mirrors = append(mirrors, mname)
		_, err = js.AddStream(&nats.StreamConfig{Name: mname, Mirror: &nats.StreamSource{Name: "S"}})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	// Queue up messages.
	numRequests := 20
	for i := 0; i < numRequests; i++ {
		js.Publish("S", []byte("Q"))
	}

	checkFor(t, 5*time.Second, 100*time.Millisecond, func() error {
		for _, mname := range mirrors {
			si, err := js.StreamInfo(mname)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if si.State.Msgs != uint64(numRequests) {
				return fmt.Errorf("Expected %d msgs for %q, got state: %+v", numRequests, mname, si.State)
			}
		}
		return nil
	})
}

// Make sure when we try to hard reset a stream state in a cluster that we also re-create the consumers.
func TestNoRaceJetStreamClusterStreamReset(t *testing.T) {
	// Speed up raft
	omin, omax, ohb := minElectionTimeout, maxElectionTimeout, hbInterval
	minElectionTimeout = 250 * time.Millisecond
	maxElectionTimeout = time.Second
	hbInterval = 50 * time.Millisecond
	defer func() {
		minElectionTimeout = omin
		maxElectionTimeout = omax
		hbInterval = ohb
	}()

	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	s := c.randomServer()
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:      "TEST",
		Subjects:  []string{"foo.*"},
		Replicas:  2,
		Retention: nats.WorkQueuePolicy,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	numRequests := 20
	for i := 0; i < numRequests; i++ {
		js.Publish("foo.created", []byte("REQ"))
	}

	// Durable.
	sub, err := js.SubscribeSync("foo.created", nats.Durable("d1"))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer sub.Unsubscribe()

	si, err := js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if si.State.Msgs != uint64(numRequests) {
		t.Fatalf("Expected %d msgs, got bad state: %+v", numRequests, si.State)
	}

	// Let settle a bit for Go routine checks.
	time.Sleep(250 * time.Millisecond)

	// Grab number go routines.
	base := runtime.NumGoroutine()

	// Make the consumer busy here by async sending a bunch of messages.
	for i := 0; i < numRequests*10; i++ {
		js.PublishAsync("foo.created", []byte("REQ"))
	}

	// Grab a server that is the consumer leader for the durable.
	cl := c.consumerLeader("$G", "TEST", "d1")
	mset, err := cl.GlobalAccount().lookupStream("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Do a hard reset here by hand.
	mset.resetClusteredState(nil)

	// Wait til we have the consumer leader re-elected.
	c.waitOnConsumerLeader("$G", "TEST", "d1")

	// So we do not wait all 10s in each call to ConsumerInfo.
	js2, _ := nc.JetStream(nats.MaxWait(250 * time.Millisecond))
	// Make sure we can get the consumer info eventually.
	checkFor(t, 5*time.Second, 200*time.Millisecond, func() error {
		_, err := js2.ConsumerInfo("TEST", "d1")
		return err
	})

	// Grab number go routines.
	if after := runtime.NumGoroutine(); base > after {
		t.Fatalf("Expected %d go routines, got %d", base, after)
	}

	// Simulate a low level write error on our consumer and make sure we can recover etc.
	cl = c.consumerLeader("$G", "TEST", "d1")
	mset, err = cl.GlobalAccount().lookupStream("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	o := mset.lookupConsumer("d1")
	if o == nil {
		t.Fatalf("Did not retrieve consumer")
	}
	node := o.raftNode().(*raft)
	if node == nil {
		t.Fatalf("could not retrieve the raft node for consumer")
	}

	nc.Close()
	node.setWriteErr(io.ErrShortWrite)

	c.stopAll()
	c.restartAll()

	c.waitOnStreamLeader("$G", "TEST")
	c.waitOnConsumerLeader("$G", "TEST", "d1")
}

// Reports of high cpu on compaction for a KV store.
func TestNoRaceJetStreamKeyValueCompaction(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   "COMPACT",
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	value := strings.Repeat("A", 128*1024)
	for i := 0; i < 5_000; i++ {
		key := fmt.Sprintf("K-%d", rand.Intn(256)+1)
		if _, err := kv.PutString(key, value); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}
}

// Trying to recreate an issue rip saw with KV and server restarts complaining about
// mismatch for a few minutes and growing memory.
func TestNoRaceJetStreamClusterStreamSeqMismatchIssue(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   "MM",
		Replicas: 3,
		TTL:      500 * time.Millisecond,
	})
	require_NoError(t, err)

	for i := 1; i <= 10; i++ {
		if _, err := kv.PutString("k", "1"); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}
	// Close in case we are connected here. Will recreate.
	nc.Close()

	// Shutdown a non-leader.
	s := c.randomNonStreamLeader("$G", "KV_MM")
	s.Shutdown()

	nc, js = jsClientConnect(t, c.randomServer())
	defer nc.Close()

	kv, err = js.KeyValue("MM")
	require_NoError(t, err)

	// Now change the state of the stream such that we have to do a compact upon restart
	// of the downed server.
	for i := 1; i <= 10; i++ {
		if _, err := kv.PutString("k", "2"); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	// Raft could save us here so need to run a compact on the leader.
	snapshotLeader := func() {
		sl := c.streamLeader("$G", "KV_MM")
		if sl == nil {
			t.Fatalf("Did not get the leader")
		}
		mset, err := sl.GlobalAccount().lookupStream("KV_MM")
		require_NoError(t, err)
		node := mset.raftNode()
		if node == nil {
			t.Fatalf("Could not get stream group")
		}
		if err := node.InstallSnapshot(mset.stateSnapshot()); err != nil {
			t.Fatalf("Error installing snapshot: %v", err)
		}
	}

	// Now wait for expiration
	time.Sleep(time.Second)

	snapshotLeader()

	s = c.restartServer(s)
	c.waitOnServerCurrent(s)

	// We want to make sure we do not reset the raft state on a catchup due to no request yield.
	// Bug was if we did not actually request any help from snapshot we did not set mset.lseq properly.
	// So when we send next batch that would cause raft reset due to cluster reset for our stream.
	mset, err := s.GlobalAccount().lookupStream("KV_MM")
	require_NoError(t, err)

	for i := 1; i <= 10; i++ {
		if _, err := kv.PutString("k1", "X"); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	c.waitOnStreamCurrent(s, "$G", "KV_MM")

	// Make sure we did not reset our stream.
	msetNew, err := s.GlobalAccount().lookupStream("KV_MM")
	require_NoError(t, err)
	if msetNew != mset {
		t.Fatalf("Stream was reset")
	}
}

func TestNoRaceJetStreamClusterStreamDropCLFS(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Client based API
	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   "CLFS",
		Replicas: 3,
	})
	require_NoError(t, err)

	// Will work
	_, err = kv.Create("k.1", []byte("X"))
	require_NoError(t, err)
	// Drive up CLFS state on leader.
	for i := 0; i < 10; i++ {
		_, err = kv.Create("k.1", []byte("X"))
		require_Error(t, err)
	}
	// Bookend with new key success.
	_, err = kv.Create("k.2", []byte("Z"))
	require_NoError(t, err)

	// Close in case we are connected here. Will recreate.
	nc.Close()

	// Shutdown, which will also clear clfs.
	s := c.randomNonStreamLeader("$G", "KV_CLFS")
	s.Shutdown()

	nc, js = jsClientConnect(t, c.randomServer())
	defer nc.Close()

	kv, err = js.KeyValue("CLFS")
	require_NoError(t, err)

	// Drive up CLFS state on leader.
	for i := 0; i < 10; i++ {
		_, err = kv.Create("k.1", []byte("X"))
		require_Error(t, err)
	}

	sl := c.streamLeader("$G", "KV_CLFS")
	if sl == nil {
		t.Fatalf("Did not get the leader")
	}
	mset, err := sl.GlobalAccount().lookupStream("KV_CLFS")
	require_NoError(t, err)
	node := mset.raftNode()
	if node == nil {
		t.Fatalf("Could not get stream group")
	}
	if err := node.InstallSnapshot(mset.stateSnapshot()); err != nil {
		t.Fatalf("Error installing snapshot: %v", err)
	}

	_, err = kv.Create("k.3", []byte("ZZZ"))
	require_NoError(t, err)

	s = c.restartServer(s)
	c.waitOnServerCurrent(s)

	mset, err = s.GlobalAccount().lookupStream("KV_CLFS")
	require_NoError(t, err)

	_, err = kv.Create("k.4", []byte("YYY"))
	require_NoError(t, err)

	c.waitOnStreamCurrent(s, "$G", "KV_CLFS")

	// Make sure we did not reset our stream.
	msetNew, err := s.GlobalAccount().lookupStream("KV_CLFS")
	require_NoError(t, err)
	if msetNew != mset {
		t.Fatalf("Stream was reset")
	}
}

func TestNoRaceJetStreamMemstoreWithLargeInteriorDeletes(t *testing.T) {
	s := RunBasicJetStreamServer()
	config := s.JetStreamConfig()
	if config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	// Client for API requests.
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:              "TEST",
		Subjects:          []string{"foo", "bar"},
		MaxMsgsPerSubject: 1,
		Storage:           nats.MemoryStorage,
	})
	require_NoError(t, err)

	acc, err := s.lookupAccount("$G")
	require_NoError(t, err)
	mset, err := acc.lookupStream("TEST")
	require_NoError(t, err)

	msg := []byte("Hello World!")
	if _, err := js.PublishAsync("foo", msg); err != nil {
		t.Fatalf("Unexpected publish error: %v", err)
	}
	for i := 1; i <= 1_000_000; i++ {
		if _, err := js.PublishAsync("bar", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	now := time.Now()
	ss := mset.stateWithDetail(true)
	// Before the fix the snapshot for this test would be > 200ms on my setup.
	if elapsed := time.Since(now); elapsed > 50*time.Millisecond {
		t.Fatalf("Took too long to snapshot: %v", elapsed)
	}

	if ss.Msgs != 2 || ss.FirstSeq != 1 || ss.LastSeq != 1_000_001 || ss.NumDeleted != 999999 {
		// To not print out on error.
		ss.Deleted = nil
		t.Fatalf("Bad State: %+v", ss)
	}
}

// This is related to an issue reported where we were exhausting threads by trying to
// cleanup too many consumers at the same time.
// https://github.com/nats-io/nats-server/issues/2742
func TestNoRaceConsumerFileStoreConcurrentDiskIO(t *testing.T) {
	storeDir := createDir(t, JetStreamStoreDir)
	defer removeDir(t, storeDir)

	// Artificially adjust our environment for this test.
	gmp := runtime.GOMAXPROCS(32)
	defer runtime.GOMAXPROCS(gmp)

	maxT := debug.SetMaxThreads(64)
	defer debug.SetMaxThreads(maxT)

	fs, err := newFileStore(FileStoreConfig{StoreDir: storeDir}, StreamConfig{Name: "MT", Storage: FileStorage})
	require_NoError(t, err)
	defer fs.Stop()

	startCh := make(chan bool)
	var wg sync.WaitGroup
	var swg sync.WaitGroup

	ts := time.Now().UnixNano()

	// Create 1000 consumerStores
	n := 1000
	swg.Add(n)

	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("o%d", i)
		o, err := fs.ConsumerStore(name, &ConsumerConfig{AckPolicy: AckExplicit})
		require_NoError(t, err)
		wg.Add(1)
		swg.Done()

		go func() {
			defer wg.Done()
			// Will make everyone run concurrently.
			<-startCh
			o.UpdateDelivered(22, 22, 1, ts)
			buf, _ := o.(*consumerFileStore).encodeState()
			o.(*consumerFileStore).writeState(buf)
			o.Delete()
		}()
	}

	swg.Wait()
	close(startCh)
	wg.Wait()
}

func TestNoRaceJetStreamClusterHealthz(t *testing.T) {
	c := createJetStreamCluster(t, jsClusterAccountsTempl, "HZ", _EMPTY_, 3, 23033, true)
	defer c.shutdown()

	nc1, js1 := jsClientConnect(t, c.randomServer(), nats.UserInfo("one", "p"))
	defer nc1.Close()

	nc2, js2 := jsClientConnect(t, c.randomServer(), nats.UserInfo("two", "p"))
	defer nc2.Close()

	var err error
	for _, sname := range []string{"foo", "bar", "baz"} {
		_, err = js1.AddStream(&nats.StreamConfig{Name: sname, Replicas: 3})
		require_NoError(t, err)
		_, err = js2.AddStream(&nats.StreamConfig{Name: sname, Replicas: 3})
		require_NoError(t, err)
	}
	// R1
	_, err = js1.AddStream(&nats.StreamConfig{Name: "r1", Replicas: 1})
	require_NoError(t, err)

	// Now shutdown then send a bunch of data.
	s := c.servers[0]
	s.Shutdown()

	for i := 0; i < 5_000; i++ {
		_, err = js1.PublishAsync("foo", []byte("OK"))
		require_NoError(t, err)
		_, err = js2.PublishAsync("bar", []byte("OK"))
		require_NoError(t, err)
	}
	select {
	case <-js1.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}
	select {
	case <-js2.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	s = c.restartServer(s)
	opts := s.getOpts()
	opts.HTTPHost = "127.0.0.1"
	opts.HTTPPort = 11222
	err = s.StartMonitoring()
	require_NoError(t, err)
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", opts.HTTPPort)

	getHealth := func() (int, *HealthStatus) {
		resp, err := http.Get(url)
		require_NoError(t, err)
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		require_NoError(t, err)
		var hs HealthStatus
		err = json.Unmarshal(body, &hs)
		require_NoError(t, err)
		return resp.StatusCode, &hs
	}

	errors := 0
	checkFor(t, 20*time.Second, 100*time.Millisecond, func() error {
		code, hs := getHealth()
		if code >= 200 && code < 300 {
			return nil
		}
		errors++
		return fmt.Errorf("Got %d status with %+v", code, hs)
	})
	if errors == 0 {
		t.Fatalf("Expected to have some errors until we became current, got none")
	}
}

// Test that we can receive larger messages with stream subject details.
// Also test that we will fail at some point and the user can fall back to
// an orderedconsumer like we do with watch for KV Keys() call.
func TestNoRaceJetStreamStreamInfoSubjectDetailsLimits(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		jetstream: enabled
		accounts: {
		  default: {
			jetstream: true
			users: [ {user: me, password: pwd} ]
			limits { max_payload: 256 }
		  }
		}
	`))
	defer removeFile(t, conf)

	s, _ := RunServerWithConfig(conf)
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s, nats.UserInfo("me", "pwd"))
	defer nc.Close()

	// Make sure we cannot send larger than 256 bytes.
	// But we can receive larger.
	sub, err := nc.SubscribeSync("foo")
	require_NoError(t, err)
	err = nc.Publish("foo", []byte(strings.Repeat("A", 300)))
	require_Error(t, err, nats.ErrMaxPayload)
	sub.Unsubscribe()

	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"*", "X.*"},
	})
	require_NoError(t, err)

	n := JSMaxSubjectDetails
	for i := 0; i < n; i++ {
		_, err := js.PublishAsync(fmt.Sprintf("X.%d", i), []byte("OK"))
		require_NoError(t, err)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	getInfo := func(filter string) *StreamInfo {
		t.Helper()
		// Need to grab StreamInfo by hand for now.
		req, err := json.Marshal(&JSApiStreamInfoRequest{SubjectsFilter: filter})
		require_NoError(t, err)
		resp, err := nc.Request(fmt.Sprintf(JSApiStreamInfoT, "TEST"), req, 5*time.Second)
		require_NoError(t, err)
		var si StreamInfo
		err = json.Unmarshal(resp.Data, &si)
		require_NoError(t, err)
		return &si
	}

	si := getInfo("X.*")
	if len(si.State.Subjects) != n {
		t.Fatalf("Expected to get %d subject details, got %d", n, len(si.State.Subjects))
	}

	// Now add one more message in which will exceed our internal limits for subject details.
	_, err = js.Publish("foo", []byte("TOO MUCH"))
	require_NoError(t, err)

	req, err := json.Marshal(&JSApiStreamInfoRequest{SubjectsFilter: nats.AllKeys})
	require_NoError(t, err)
	resp, err := nc.Request(fmt.Sprintf(JSApiStreamInfoT, "TEST"), req, 5*time.Second)
	require_NoError(t, err)
	var sir JSApiStreamInfoResponse
	err = json.Unmarshal(resp.Data, &sir)
	require_NoError(t, err)
	if !IsNatsErr(sir.Error, JSStreamInfoMaxSubjectsErr) {
		t.Fatalf("Did not get correct error response: %+v", sir.Error)
	}
}

func TestNoRaceJetStreamSparseConsumers(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	msg := []byte("ok")

	cases := []struct {
		name    string
		mconfig *nats.StreamConfig
	}{
		{"MemoryStore", &nats.StreamConfig{Name: "TEST", Storage: nats.MemoryStorage, MaxMsgsPerSubject: 25_000_000,
			Subjects: []string{"*"}}},
		{"FileStore", &nats.StreamConfig{Name: "TEST", Storage: nats.FileStorage, MaxMsgsPerSubject: 25_000_000,
			Subjects: []string{"*"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			js.DeleteStream("TEST")
			_, err := js.AddStream(c.mconfig)
			require_NoError(t, err)

			// We will purposely place foo msgs near the beginning, then in middle, then at the end.
			for n := 0; n < 2; n++ {
				_, err = js.PublishAsync("foo", msg)
				require_NoError(t, err)

				for i := 0; i < 1_000_000; i++ {
					_, err = js.PublishAsync("bar", msg)
					require_NoError(t, err)
				}
				_, err = js.PublishAsync("foo", msg)
				require_NoError(t, err)
			}
			select {
			case <-js.PublishAsyncComplete():
			case <-time.After(5 * time.Second):
				t.Fatalf("Did not receive completion signal")
			}

			// Now create a consumer on foo.
			ci, err := js.AddConsumer("TEST", &nats.ConsumerConfig{DeliverSubject: "x.x", FilterSubject: "foo", AckPolicy: nats.AckNonePolicy})
			require_NoError(t, err)

			done, received := make(chan bool), uint64(0)

			cb := func(m *nats.Msg) {
				received++
				if received >= ci.NumPending {
					done <- true
				}
			}

			sub, err := nc.Subscribe("x.x", cb)
			require_NoError(t, err)
			defer sub.Unsubscribe()
			start := time.Now()
			var elapsed time.Duration

			select {
			case <-done:
				elapsed = time.Since(start)
			case <-time.After(10 * time.Second):
				t.Fatal("Did not receive all messages for all consumers in time")
			}

			if elapsed > 500*time.Millisecond {
				t.Fatalf("Getting all messages took longer than expected: %v", elapsed)
			}
		})
	}
}

func TestNoRaceJetStreamConsumerFilterPerfDegradation(t *testing.T) {
	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, _ := jsClientConnect(t, s)
	defer nc.Close()

	js, err := nc.JetStream(nats.PublishAsyncMaxPending(256))
	require_NoError(t, err)

	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "test",
		Subjects: []string{"test.*.subj"},
		Replicas: 1,
	})
	require_NoError(t, err)

	toSend := 50_000
	count := 0
	ch := make(chan struct{}, 6)
	_, err = js.Subscribe("test.*.subj", func(m *nats.Msg) {
		m.Ack()
		if count++; count == toSend {
			ch <- struct{}{}
		}
	}, nats.DeliverNew(), nats.ManualAck())
	require_NoError(t, err)

	msg := make([]byte, 1024)
	sent := int32(0)
	send := func() {
		defer func() { ch <- struct{}{} }()
		for i := 0; i < toSend/5; i++ {
			msgID := atomic.AddInt32(&sent, 1)
			_, err := js.Publish(fmt.Sprintf("test.%d.subj", msgID), msg)
			if err != nil {
				t.Error(err)
				return
			}
		}
	}
	for i := 0; i < 5; i++ {
		go send()
	}
	timeout := time.NewTimer(10 * time.Second)
	for i := 0; i < 6; i++ {
		select {
		case <-ch:
		case <-timeout.C:
			t.Fatal("Took too long")
		}
	}
}

func TestNoRaceFileStoreSubjectInfoWithSnapshotCleanup(t *testing.T) {
	storeDir := createDir(t, JetStreamStoreDir)
	defer removeDir(t, storeDir)

	fs, err := newFileStore(FileStoreConfig{StoreDir: storeDir, BlockSize: 1024 * 1024}, StreamConfig{Name: "TEST", Storage: FileStorage})
	require_NoError(t, err)
	defer fs.Stop()

	n, msg := 10_000, []byte(strings.Repeat("Z", 1024))
	for i := 0; i < n; i++ {
		_, _, err := fs.StoreMsg(fmt.Sprintf("X.%d", i), nil, msg)
		require_NoError(t, err)
	}

	// Snapshot causes us to write out per subject info, fss files.
	// We want to make sure they get cleaned up.
	sr, err := fs.Snapshot(5*time.Second, false, false)
	require_NoError(t, err)
	var buf [4 * 1024 * 1024]byte
	for {
		if _, err = sr.Reader.Read(buf[:]); err == io.EOF {
			break
		}
		require_NoError(t, err)
	}

	var seqs []uint64
	for i := 1; i <= n; i++ {
		seqs = append(seqs, uint64(i))
	}
	// Randomly delete msgs, make sure we cleanup as we empty the message blocks.
	rand.Shuffle(len(seqs), func(i, j int) { seqs[i], seqs[j] = seqs[j], seqs[i] })

	for _, seq := range seqs {
		_, err := fs.RemoveMsg(seq)
		require_NoError(t, err)
	}

	// We will have cleanup the main .blk and .idx sans the lmb, but we should not have any *.fss files.
	fms, err := filepath.Glob(filepath.Join(storeDir, msgDir, fssScanAll))
	require_NoError(t, err)

	if len(fms) > 0 {
		t.Fatalf("Expected to find no fss files, found %d", len(fms))
	}
}

func TestNoRaceFileStoreKeyFileCleanup(t *testing.T) {
	storeDir := createDir(t, JetStreamStoreDir)
	defer removeDir(t, storeDir)

	prf := func(context []byte) ([]byte, error) {
		h := hmac.New(sha256.New, []byte("dlc22"))
		if _, err := h.Write(context); err != nil {
			return nil, err
		}
		return h.Sum(nil), nil
	}

	fs, err := newFileStoreWithCreated(
		FileStoreConfig{StoreDir: storeDir, BlockSize: 1024 * 1024},
		StreamConfig{Name: "TEST", Storage: FileStorage},
		time.Now(),
		prf)
	require_NoError(t, err)
	defer fs.Stop()

	n, msg := 10_000, []byte(strings.Repeat("Z", 1024))
	for i := 0; i < n; i++ {
		_, _, err := fs.StoreMsg(fmt.Sprintf("X.%d", i), nil, msg)
		require_NoError(t, err)
	}

	var seqs []uint64
	for i := 1; i <= n; i++ {
		seqs = append(seqs, uint64(i))
	}
	// Randomly delete msgs, make sure we cleanup as we empty the message blocks.
	rand.Shuffle(len(seqs), func(i, j int) { seqs[i], seqs[j] = seqs[j], seqs[i] })

	for _, seq := range seqs {
		_, err := fs.RemoveMsg(seq)
		require_NoError(t, err)
	}

	// We will have cleanup the main .blk and .idx sans the lmb, but we should not have any *.fss files.
	kms, err := filepath.Glob(filepath.Join(storeDir, msgDir, keyScanAll))
	require_NoError(t, err)

	if len(kms) > 1 {
		t.Fatalf("Expected to find only 1 key file, found %d", len(kms))
	}
}

func TestNoRaceMsgIdPerfDuringCatchup(t *testing.T) {
	// Uncomment to run. Needs to be on a bigger machine. Do not want as part of Travis tests atm.
	skip(t)

	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.serverByName("S-1"))
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Replicas: 3,
	})
	require_NoError(t, err)

	// This will be the one we restart.
	sl := c.streamLeader("$G", "TEST")
	// Now move leader.
	_, err = nc.Request(fmt.Sprintf(JSApiStreamLeaderStepDownT, "TEST"), nil, time.Second)
	require_NoError(t, err)
	c.waitOnStreamLeader("$G", "TEST")

	// Connect to new leader.
	nc, _ = jsClientConnect(t, c.streamLeader("$G", "TEST"))
	defer nc.Close()

	js, err = nc.JetStream(nats.PublishAsyncMaxPending(1024))
	require_NoError(t, err)

	n, ss, sr := 1_000_000, 250_000, 800_000
	m := nats.NewMsg("TEST")
	m.Data = []byte(strings.Repeat("Z", 2048))

	// Target rate 10k msgs/sec
	start := time.Now()

	for i := 0; i < n; i++ {
		m.Header.Set(JSMsgId, strconv.Itoa(i))
		_, err := js.PublishMsgAsync(m)
		require_NoError(t, err)
		//time.Sleep(42 * time.Microsecond)
		if i == ss {
			fmt.Printf("SD")
			sl.Shutdown()
		} else if i == sr {
			nc.Flush()
			select {
			case <-js.PublishAsyncComplete():
			case <-time.After(10 * time.Second):
			}
			fmt.Printf("RS")
			sl = c.restartServer(sl)
		}
		if i%10_000 == 0 {
			fmt.Print("#")
		}
	}
	fmt.Println()

	// Wait to receive all messages.
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(20 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	tt := time.Since(start)
	si, err := js.StreamInfo("TEST")
	require_NoError(t, err)

	fmt.Printf("Took %v to send %d msgs\n", tt, n)
	fmt.Printf("%.0f msgs/s\n", float64(n)/tt.Seconds())
	fmt.Printf("%.0f mb/s\n\n", float64(si.State.Bytes/(1024*1024))/tt.Seconds())

	c.waitOnStreamCurrent(sl, "$G", "TEST")
	for _, s := range c.servers {
		mset, _ := s.GlobalAccount().lookupStream("TEST")
		if state := mset.store.State(); state.Msgs != uint64(n) {
			t.Fatalf("Expected server %v to have correct number of msgs %d but got %d", s, n, state.Msgs)
		}
	}
}

func TestNoRaceRebuildDeDupeAndMemoryPerf(t *testing.T) {
	skip(t)

	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{Name: "DD"})
	require_NoError(t, err)

	m := nats.NewMsg("DD")
	m.Data = []byte(strings.Repeat("Z", 2048))

	start := time.Now()

	n := 1_000_000
	for i := 0; i < n; i++ {
		m.Header.Set(JSMsgId, strconv.Itoa(i))
		_, err := js.PublishMsgAsync(m)
		require_NoError(t, err)
	}

	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(20 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	tt := time.Since(start)
	si, err := js.StreamInfo("DD")
	require_NoError(t, err)

	fmt.Printf("Took %v to send %d msgs\n", tt, n)
	fmt.Printf("%.0f msgs/s\n", float64(n)/tt.Seconds())
	fmt.Printf("%.0f mb/s\n\n", float64(si.State.Bytes/(1024*1024))/tt.Seconds())

	v, _ := s.Varz(nil)
	fmt.Printf("Memory AFTER SEND: %v\n", friendlyBytes(v.Mem))

	mset, err := s.GlobalAccount().lookupStream("DD")
	require_NoError(t, err)

	mset.mu.Lock()
	mset.ddloaded = false
	start = time.Now()
	mset.rebuildDedupe()
	fmt.Printf("TOOK %v to rebuild dd\n", time.Since(start))
	mset.mu.Unlock()

	v, _ = s.Varz(nil)
	fmt.Printf("Memory: %v\n", friendlyBytes(v.Mem))

	// Now do an ephemeral consumer and whip through every message. Doing same calculations.
	start = time.Now()
	received, done := 0, make(chan bool)
	sub, err := js.Subscribe("DD", func(m *nats.Msg) {
		received++
		if received >= n {
			done <- true
		}
	}, nats.OrderedConsumer())
	require_NoError(t, err)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if s.NumSlowConsumers() > 0 {
			t.Fatalf("Did not receive all large messages due to slow consumer status: %d of %d", received, n)
		}
		t.Fatalf("Failed to receive all large messages: %d of %d\n", received, n)
	}

	fmt.Printf("TOOK %v to receive all %d msgs\n", time.Since(start), n)
	sub.Unsubscribe()

	v, _ = s.Varz(nil)
	fmt.Printf("Memory: %v\n", friendlyBytes(v.Mem))
}

func TestNoRaceMemoryUsageOnLimitedStreamWithMirror(t *testing.T) {
	skip(t)

	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{Name: "DD", Subjects: []string{"ORDERS.*"}, MaxMsgs: 10_000})
	require_NoError(t, err)

	_, err = js.AddStream(&nats.StreamConfig{
		Name:    "M",
		Mirror:  &nats.StreamSource{Name: "DD"},
		MaxMsgs: 10_000,
	})
	require_NoError(t, err)

	m := nats.NewMsg("ORDERS.0")
	m.Data = []byte(strings.Repeat("Z", 2048))

	start := time.Now()

	n := 1_000_000
	for i := 0; i < n; i++ {
		m.Subject = fmt.Sprintf("ORDERS.%d", i)
		m.Header.Set(JSMsgId, strconv.Itoa(i))
		_, err := js.PublishMsgAsync(m)
		require_NoError(t, err)
	}

	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(20 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	tt := time.Since(start)
	si, err := js.StreamInfo("DD")
	require_NoError(t, err)

	fmt.Printf("Took %v to send %d msgs\n", tt, n)
	fmt.Printf("%.0f msgs/s\n", float64(n)/tt.Seconds())
	fmt.Printf("%.0f mb/s\n\n", float64(si.State.Bytes/(1024*1024))/tt.Seconds())

	v, _ := s.Varz(nil)
	fmt.Printf("Memory AFTER SEND: %v\n", friendlyBytes(v.Mem))
}

func TestNoRaceOrderedConsumerLongRTTPerformance(t *testing.T) {
	skip(t)

	s := RunBasicJetStreamServer()
	if config := s.JetStreamConfig(); config != nil {
		defer removeDir(t, config.StoreDir)
	}
	defer s.Shutdown()

	nc, _ := jsClientConnect(t, s)
	defer nc.Close()

	js, err := nc.JetStream(nats.PublishAsyncMaxPending(1000))
	require_NoError(t, err)

	_, err = js.AddStream(&nats.StreamConfig{Name: "OCP"})
	require_NoError(t, err)

	n, msg := 100_000, []byte(strings.Repeat("D", 30_000))

	for i := 0; i < n; i++ {
		_, err := js.PublishAsync("OCP", msg)
		require_NoError(t, err)
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(5 * time.Second):
		t.Fatalf("Did not receive completion signal")
	}

	// Approximately 3GB
	si, err := js.StreamInfo("OCP")
	require_NoError(t, err)

	start := time.Now()
	received, done := 0, make(chan bool)
	sub, err := js.Subscribe("OCP", func(m *nats.Msg) {
		received++
		if received >= n {
			done <- true
		}
	}, nats.OrderedConsumer())
	require_NoError(t, err)
	defer sub.Unsubscribe()

	// Wait to receive all messages.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("Did not receive all of our messages")
	}

	tt := time.Since(start)
	fmt.Printf("Took %v to receive %d msgs\n", tt, n)
	fmt.Printf("%.0f msgs/s\n", float64(n)/tt.Seconds())
	fmt.Printf("%.0f mb/s\n\n", float64(si.State.Bytes/(1024*1024))/tt.Seconds())

	sub.Unsubscribe()

	rtt := 10 * time.Millisecond
	bw := 10 * 1024 * 1024 * 1024
	proxy := newNetProxy(rtt, bw, bw, s.ClientURL())
	defer proxy.stop()

	nc, err = nats.Connect(proxy.clientURL())
	require_NoError(t, err)
	js, err = nc.JetStream()
	require_NoError(t, err)

	start, received = time.Now(), 0
	sub, err = js.Subscribe("OCP", func(m *nats.Msg) {
		received++
		if received >= n {
			done <- true
		}
	}, nats.OrderedConsumer())
	require_NoError(t, err)
	defer sub.Unsubscribe()

	// Wait to receive all messages.
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("Did not receive all of our messages")
	}

	tt = time.Since(start)
	fmt.Printf("Proxy RTT: %v, UP: %d, DOWN: %d\n", rtt, bw, bw)
	fmt.Printf("Took %v to receive %d msgs\n", tt, n)
	fmt.Printf("%.0f msgs/s\n", float64(n)/tt.Seconds())
	fmt.Printf("%.0f mb/s\n\n", float64(si.State.Bytes/(1024*1024))/tt.Seconds())
}

var jsClusterStallCatchupTempl = `
	listen: 127.0.0.1:-1
	server_name: %s
	jetstream: {max_mem_store: 256MB, max_file_store: 32GB, store_dir: '%s'}

	leaf {
		listen: 127.0.0.1:-1
	}

	cluster {
		name: %s
		listen: 127.0.0.1:%d
		routes = [%s]
	}

	# For access to system account.
	accounts { $SYS { users = [ { user: "admin", pass: "s3cr3t!" } ] } }
`

// Test our global stall gate for outstanding catchup bytes.
func TestNoRaceJetStreamClusterCatchupStallGate(t *testing.T) {
	skip(t)

	c := createJetStreamClusterWithTemplate(t, jsClusterStallCatchupTempl, "GSG", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	// ~100k per message.
	msg := []byte(strings.Repeat("A", 99_960))

	// Create 200 streams with 100MB.
	// Each server has ~2GB
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(x int) {
			defer wg.Done()
			for n := 1; n <= 10; n++ {
				sn := fmt.Sprintf("S-%d", n+x)
				_, err := js.AddStream(&nats.StreamConfig{
					Name:     sn,
					Replicas: 3,
				})
				require_NoError(t, err)
				for i := 0; i < 100; i++ {
					_, err := js.Publish(sn, msg)
					require_NoError(t, err)
				}
			}
		}(i * 20)
	}
	wg.Wait()

	info, err := js.AccountInfo()
	require_NoError(t, err)
	require_True(t, info.Streams == 200)

	runtime.GC()
	debug.FreeOSMemory()

	// Now bring a server down and wipe its storage.
	s := c.servers[0]
	vz, err := s.Varz(nil)
	require_NoError(t, err)
	fmt.Printf("MEM BEFORE is %v\n", friendlyBytes(vz.Mem))

	sd := s.JetStreamConfig().StoreDir
	s.Shutdown()
	removeDir(t, sd)
	s = c.restartServer(s)

	c.waitOnServerHealthz(s)

	runtime.GC()
	debug.FreeOSMemory()

	vz, err = s.Varz(nil)
	require_NoError(t, err)
	fmt.Printf("MEM AFTER is %v\n", friendlyBytes(vz.Mem))
}

func TestNoRaceJetStreamClusterCatchupBailMidway(t *testing.T) {
	skip(t)

	c := createJetStreamClusterWithTemplate(t, jsClusterStallCatchupTempl, "GSG", 3)
	defer c.shutdown()

	ml := c.leader()
	nc, js := jsClientConnect(t, ml)
	defer nc.Close()

	msg := []byte(strings.Repeat("A", 480))

	for i := 0; i < maxConcurrentSyncRequests*2; i++ {
		sn := fmt.Sprintf("CUP-%d", i+1)
		_, err := js.AddStream(&nats.StreamConfig{
			Name:     sn,
			Replicas: 3,
		})
		require_NoError(t, err)

		for i := 0; i < 10_000; i++ {
			_, err := js.PublishAsync(sn, msg)
			require_NoError(t, err)
		}
		select {
		case <-js.PublishAsyncComplete():
		case <-time.After(10 * time.Second):
			t.Fatalf("Did not receive completion signal")
		}
	}

	jsz, _ := ml.Jsz(nil)
	expectedMsgs := jsz.Messages

	// Now select a server and shut it down, removing the storage directory.
	s := c.randomNonLeader()
	sd := s.JetStreamConfig().StoreDir
	s.Shutdown()
	removeDir(t, sd)

	// Now restart the server.
	s = c.restartServer(s)

	// We want to force the follower to bail before the catchup through the
	// upper level catchup logic completes.
	checkFor(t, 5*time.Second, 10*time.Millisecond, func() error {
		jsz, _ := s.Jsz(nil)
		if jsz.Messages > expectedMsgs/2 {
			s.Shutdown()
			return nil
		}
		return fmt.Errorf("Not enough yet")
	})

	// Now restart the server.
	s = c.restartServer(s)

	checkFor(t, 5*time.Second, 500*time.Millisecond, func() error {
		jsz, _ := s.Jsz(nil)
		if jsz.Messages == expectedMsgs {
			return nil
		}
		return fmt.Errorf("Not enough yet")
	})
}

func TestNoRaceJetStreamAccountLimitsAndRestart(t *testing.T) {
	c := createJetStreamClusterWithTemplate(t, jsClusterAccountLimitsTempl, "A3S", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	if _, err := js.AddStream(&nats.StreamConfig{Name: "TEST", Replicas: 3}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	for i := 0; i < 20_000; i++ {
		if _, err := js.Publish("TEST", []byte("A")); err != nil {
			break
		}
		if i == 5_000 {
			snl := c.randomNonStreamLeader("$JS", "TEST")
			snl.Shutdown()
		}
	}

	c.stopAll()
	c.restartAll()
	c.waitOnLeader()
	c.waitOnStreamLeader("$JS", "TEST")

	for _, cs := range c.servers {
		c.waitOnStreamCurrent(cs, "$JS", "TEST")
	}
}

func TestNoRaceJetStreamPullConsumersAndInteriorDeletes(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "ID", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:      "foo",
		Replicas:  3,
		MaxMsgs:   50000,
		Retention: nats.InterestPolicy,
	})
	require_NoError(t, err)

	c.waitOnStreamLeader(globalAccountName, "foo")

	_, err = js.AddConsumer("foo", &nats.ConsumerConfig{
		Durable:       "foo",
		FilterSubject: "foo",
		MaxAckPending: 20000,
		AckWait:       time.Minute,
		AckPolicy:     nats.AckExplicitPolicy,
	})
	require_NoError(t, err)

	c.waitOnConsumerLeader(globalAccountName, "foo", "foo")

	rcv := int32(0)
	prods := 5
	cons := 5
	wg := sync.WaitGroup{}
	wg.Add(prods + cons)
	toSend := 100000

	for i := 0; i < cons; i++ {
		go func() {
			defer wg.Done()

			sub, err := js.PullSubscribe("foo", "foo")
			if err != nil {
				return
			}
			for {
				msgs, err := sub.Fetch(200, nats.MaxWait(250*time.Millisecond))
				if err != nil {
					if n := int(atomic.LoadInt32(&rcv)); n >= toSend {
						return
					}
					continue
				}
				for _, m := range msgs {
					m.Ack()
					atomic.AddInt32(&rcv, 1)
				}
			}
		}()
	}

	for i := 0; i < prods; i++ {
		go func() {
			defer wg.Done()

			for i := 0; i < toSend/prods; i++ {
				js.Publish("foo", []byte("hello"))
			}
		}()
	}

	time.Sleep(time.Second)
	resp, err := nc.Request(fmt.Sprintf(JSApiConsumerLeaderStepDownT, "foo", "foo"), nil, time.Second)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	var cdResp JSApiConsumerLeaderStepDownResponse
	if err := json.Unmarshal(resp.Data, &cdResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if cdResp.Error != nil {
		t.Fatalf("Unexpected error: %+v", cdResp.Error)
	}
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
		// OK
	case <-time.After(20 * time.Second):
		t.Fatalf("Consumers took too long to consumer all messages")
	}
}

func TestNoRaceJetStreamClusterInterestPullConsumerStreamLimitBug(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	nc, js := jsClientConnect(t, c.randomServer())
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:      "TEST",
		Subjects:  []string{"foo"},
		Retention: nats.InterestPolicy,
		MaxMsgs:   2000,
		Replicas:  3,
	})
	require_NoError(t, err)

	_, err = js.AddConsumer("TEST", &nats.ConsumerConfig{Durable: "dur", AckPolicy: nats.AckExplicitPolicy})
	require_NoError(t, err)

	qch := make(chan bool)
	var wg sync.WaitGroup

	// Publisher
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			pt := time.NewTimer(time.Duration(rand.Intn(2)) * time.Millisecond)
			select {
			case <-pt.C:
				_, err := js.Publish("foo", []byte("BUG!"))
				if err != nil {
					t.Logf("Got a publisher error: %v", err)
					return
				}
			case <-qch:
				return
			}
		}
	}()

	time.Sleep(time.Second)

	// Pull Consumers
	wg.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			_, js := jsClientConnect(t, c.randomServer())
			sub, err := js.PullSubscribe("foo", "dur")
			require_NoError(t, err)

			for {
				pt := time.NewTimer(time.Duration(rand.Intn(300)) * time.Millisecond)
				select {
				case <-pt.C:
					msgs, err := sub.Fetch(1)
					if err != nil {
						t.Logf("Got a Fetch error: %v", err)
						return
					}
					if len(msgs) > 0 {
						go func() {
							ackDelay := time.Duration(rand.Intn(375)+15) * time.Millisecond
							m := msgs[0]
							time.AfterFunc(ackDelay, func() { m.AckSync() })
						}()
					}
				case <-qch:
					return
				}
			}
		}()
	}

	time.Sleep(5 * time.Second)
	close(qch)
	wg.Wait()
	time.Sleep(time.Second)

	si, err := js.StreamInfo("TEST")
	require_NoError(t, err)

	ci, err := js.ConsumerInfo("TEST", "dur")
	require_NoError(t, err)

	ld := ci.Delivered.Stream
	if si.State.FirstSeq > ld {
		ld = si.State.FirstSeq - 1
	}
	if si.State.LastSeq-ld != ci.NumPending {
		t.Fatalf("Expected NumPending to be %d got %d", si.State.LastSeq-ld, ci.NumPending)
	}
}

// Net Proxy - For introducing RTT and BW constraints.
type netProxy struct {
	listener net.Listener
	conns    []net.Conn
	url      string
}

func newNetProxy(rtt time.Duration, upRate, downRate int, serverURL string) *netProxy {
	hp := net.JoinHostPort("127.0.0.1", "0")
	l, e := net.Listen("tcp", hp)
	if e != nil {
		panic(fmt.Sprintf("Error listening on port: %s, %q", hp, e))
	}
	port := l.Addr().(*net.TCPAddr).Port
	proxy := &netProxy{listener: l}
	go func() {
		client, err := l.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", serverURL[7:], time.Second)
		if err != nil {
			panic("Can't connect to NATS server")
		}
		proxy.conns = append(proxy.conns, client, server)
		go proxy.loop(rtt, upRate, client, server)
		go proxy.loop(rtt, downRate, server, client)
	}()
	proxy.url = fmt.Sprintf("nats://127.0.0.1:%d", port)
	return proxy
}

func (np *netProxy) clientURL() string {
	return np.url
}

func (np *netProxy) loop(rtt time.Duration, tbw int, r, w net.Conn) {
	delay := rtt / 2
	const rbl = 8192
	var buf [rbl]byte
	ctx := context.Background()

	rl := rate.NewLimiter(rate.Limit(tbw), rbl)

	for fr := true; ; {
		sr := time.Now()
		n, err := r.Read(buf[:])
		if err != nil {
			return
		}
		// RTT delays
		if fr || time.Since(sr) > 2*time.Millisecond {
			fr = false
			if delay > 0 {
				time.Sleep(delay)
			}
		}
		if err := rl.WaitN(ctx, n); err != nil {
			return
		}
		if _, err = w.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (np *netProxy) stop() {
	if np.listener != nil {
		np.listener.Close()
		np.listener = nil
		for _, c := range np.conns {
			c.Close()
		}
	}
}
