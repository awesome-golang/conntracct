// +build integration

package bpf

import (
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gitlab.com/0ptr/conntracct/pkg/udpecho"
	"golang.org/x/sys/unix"
)

// Mock UDP server listen port.
const (
	udpServ = 1342
	cd      = 20
)

var (
	acctProbe      *AcctProbe
	errChanTimeout = errors.New("timeout")
)

func TestMain(m *testing.M) {

	var err error

	cfg := AcctConfig{
		// One update every n milliseconds after startup burst.
		// Should be short enough to fit in a test window,
		// but long enough to allow startup burst to occur without
		// injecting unwanted events. (eg. on slower machines)
		CooldownMillis: cd,
	}

	// Set the required sysctl's for the probe to gather accounting data.
	err = Sysctls(false)
	if err != nil {
		log.Fatal(err)
	}

	// Create and start the AcctProbe.
	acctProbe, err = NewAcctProbe(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := acctProbe.Start(); err != nil {
		log.Fatal(err)
	}
	go errWorker(acctProbe.ErrChan())

	// Create and start the localhost UDP listener.
	c := udpecho.ListenAndEcho(1342)

	// Run tests, save the return code.
	rc := m.Run()

	// Tear down resources.
	acctProbe.Stop()
	c.Close()

	os.Exit(rc)
}

// Verifies the 'connection startup burst' behaviour of the BPF program,
// namely logging packet 1, 2, 8 and 32 of a flow.
func TestAcctProbeStartup(t *testing.T) {

	// Create and register consumer.
	ac, in := newConsumer(t)
	defer ac.Close()

	// Create UDP client.
	mc := udpecho.Dial(udpServ)

	// Filter BPF AcctEvents based on client port.
	out := filterSourcePort(in, mc.ClientPort())

	// packets 1 and 2
	// Events are sometimes delivered to userspace out of order.
	// Since the initial 2 events are delivered very closely to each other,
	// don't check their packet counts.
	mc.Ping(1)
	_, err := readTimeout(out, 10)
	require.NoError(t, err)
	_, err = readTimeout(out, 10)
	require.NoError(t, err)

	// packet 8
	mc.Ping(3)
	ev, err := readTimeout(out, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 8, ev.PacketsOrig+ev.PacketsRet, ev.String())

	// packet 32
	mc.Ping(12)
	ev, err = readTimeout(out, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 32, ev.PacketsOrig+ev.PacketsRet, ev.String())

	// Further attempt(s) to read from the channel should time out.
	ev, err = readTimeout(out, 10)
	assert.EqualError(t, err, "timeout", ev.String())

	require.NoError(t, acctProbe.RemoveConsumer(ac))
}

// Runs past the 'connection startup burst' events and tries to obtain two
// events generated by sending packets right after the flow's cooldown timer
// expires.
func TestAcctProbeLongterm(t *testing.T) {

	// Create and register consumer.
	ac, in := newConsumer(t)
	defer ac.Close()

	// Create UDP client.
	mc := udpecho.Dial(udpServ)

	// Filter BPF AcctEvents based on client port.
	out := filterSourcePort(in, mc.ClientPort())

	// Clear the connection startup burst to make sure it's not interfering
	// with our test suite. Send 16 two-way packets and read 4 events.
	mc.Ping(16)
	for i := 0; i < 4; i++ {
		_, err := readTimeout(out, 10)
		assert.NoError(t, err)
	}

	// Ensure all events are drained.
	ev, err := readTimeout(out, 20)
	assert.EqualError(t, err, "timeout", ev.String())

	// Wait for at least one cooldown period, send a one-way packet.
	time.Sleep(cd * time.Millisecond)
	mc.Nop(1)

	// Expect 33rd packet in this message.
	ev, err = readTimeout(out, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 33, ev.PacketsOrig+ev.PacketsRet, ev.String())

	// Expect 34th packet in this message.
	time.Sleep(cd * time.Millisecond)
	mc.Nop(1)
	ev, err = readTimeout(out, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 34, ev.PacketsOrig+ev.PacketsRet, ev.String())

	// Further attempt(s) to read from the channel should time out.
	ev, err = readTimeout(out, 10)
	assert.EqualError(t, err, "timeout", ev.String())

	require.NoError(t, acctProbe.RemoveConsumer(ac))
}

// Verify as many fields as possible based on information obtained from other
// sources. This checks whether the BPF program is reading the correct offsets
// from kernel memory.
func TestAcctProbeVerify(t *testing.T) {

	// Create and register consumer.
	ac, in := newConsumer(t)
	defer ac.Close()

	// Create UDP client.
	mc := udpecho.Dial(udpServ)

	// Filter BPF AcctEvents based on client port.
	out := filterSourcePort(in, mc.ClientPort())

	// Generate a single dummy event.
	mc.Nop(1)
	ev, err := readTimeout(out, 20)
	require.NoError(t, err)

	// Network Namespace
	ns, err := getNSID()
	require.NoError(t, err)
	assert.EqualValues(t, ns, ev.NetNS, ev.String())

	// Connmark (default 0)
	assert.EqualValues(t, 0, ev.Connmark, ev.String())

	// Accounting
	assert.EqualValues(t, 1, ev.PacketsOrig, ev.String())
	assert.EqualValues(t, 31, ev.BytesOrig, ev.String())
	assert.EqualValues(t, 0, ev.PacketsRet, ev.String())
	assert.EqualValues(t, 0, ev.BytesRet, ev.String())

	// Connection tuple
	assert.EqualValues(t, udpServ, ev.DstPort, ev.String())
	assert.EqualValues(t, mc.ClientPort(), ev.SrcPort, ev.String())
	assert.EqualValues(t, net.IPv4(127, 0, 0, 1), ev.SrcAddr, ev.String())
	assert.EqualValues(t, net.IPv4(127, 0, 0, 1), ev.DstAddr, ev.String())
	assert.EqualValues(t, 17, ev.Proto, ev.String())

	require.NoError(t, acctProbe.RemoveConsumer(ac))
}

// filterSourcePort returns an unbuffered channel of AcctEvents
// that has its event stream filtered by the given source port.
func filterSourcePort(in chan AcctEvent, port uint16) chan AcctEvent {
	out := make(chan AcctEvent)
	go filterWorker(in, out,
		func(ev AcctEvent) bool {
			if ev.SrcPort == port {
				return true
			}
			return false
		})

	return out
}

// filterWorker sends an AcctEvent from in to out if the given function f yields true.
func filterWorker(in <-chan AcctEvent, out chan<- AcctEvent, f func(AcctEvent) bool) {
	for {
		ev, ok := <-in
		if !ok {
			close(out)
			return
		}

		if f(ev) {
			out <- ev
		}
	}
}

// errWorker listens for errors on the AcctProbe's error channel.
// Terminates the test suite when an error occurs.
func errWorker(ec <-chan error) {
	for err := range ec {
		log.Fatal("unexpected error from AcctProbe:", err)
	}
}

// readTimeout attempts a read from an AcctEvent channel, timing out
// when a message wasn't read after ms milliseconds.
func readTimeout(c <-chan AcctEvent, ms uint) (AcctEvent, error) {
	select {
	case ev := <-c:
		return ev, nil
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return AcctEvent{}, errChanTimeout
	}
}

// newConsumer creates and registers an AcctConsumer for a test.
func newConsumer(t *testing.T) (*AcctConsumer, chan AcctEvent) {
	c := make(chan AcctEvent, 2048)
	ac := NewAcctConsumer(t.Name(), c)
	require.NoError(t, acctProbe.RegisterConsumer(ac))

	return ac, c
}

// getNSID gets the inode of the current process' network namespace.
func getNSID() (uint64, error) {
	path := fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), syscall.Gettid())
	fd, err := unix.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}

	var s unix.Stat_t
	if err := unix.Fstat(fd, &s); err != nil {
		return 0, err
	}

	return s.Ino, nil
}
