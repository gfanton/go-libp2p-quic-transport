package libp2pquic

import (
	"bytes"
	mrand "math/rand"
	"runtime/pprof"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestLibp2pQuicTransport(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "libp2p QUIC Transport Suite")
}

var _ = BeforeSuite(func() {
	mrand.Seed(GinkgoRandomSeed())
})

var garbageCollectIntervalOrig time.Duration
var maxUnusedDurationOrig time.Duration

func isGarbageCollectorRunning() bool {
	var b bytes.Buffer
	pprof.Lookup("goroutine").WriteTo(&b, 1)
	return strings.Contains(b.String(), "go-libp2p-quic-transport.(*reuse).runGarbageCollector")
}

var _ = BeforeEach(func() {
	Expect(isGarbageCollectorRunning()).To(BeFalse())
	garbageCollectIntervalOrig = garbageCollectInterval
	maxUnusedDurationOrig = maxUnusedDuration
	garbageCollectInterval = 50 * time.Millisecond
	maxUnusedDuration = 0
})

var _ = AfterEach(func() {
	Eventually(isGarbageCollectorRunning).Should(BeFalse())
	garbageCollectInterval = garbageCollectIntervalOrig
	maxUnusedDuration = maxUnusedDurationOrig
})
