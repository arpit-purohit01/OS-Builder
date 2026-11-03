package sandbox

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"foundry.dev/foundry/internal/egress"
)

// TestVMSelfTest boots the golden builder image (FOUNDRY_BUILDER_IMAGE) and
// checks the isolation contract: the VM reaches allowlisted mirrors only via
// the egress proxy, a non-allowlisted host is refused, direct egress fails.
func TestVMSelfTest(t *testing.T) {
	img := os.Getenv("FOUNDRY_BUILDER_IMAGE")
	if img == "" {
		t.Skip("FOUNDRY_BUILDER_IMAGE not set")
	}
	var mu sync.Mutex
	var denied []string
	px := &egress.Proxy{Allow: egress.DefaultAllowlist, Secret: []byte("test"), DenyPrivate: true,
		OnEvent: func(e egress.Event) {
			if !e.Allowed {
				mu.Lock()
				denied = append(denied, e.Host)
				mu.Unlock()
			}
		}}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, px)
	user, pass := px.Credentials("bld_selftest")
	v := &VM{Image: img, WorkDir: t.TempDir(), MemoryMiB: 2048, CPUs: 2, ScratchGiB: 2, OutGiB: 1, Timeout: 20 * time.Minute,
		PipeCmd: []string{"nc", "-N"}, NoSeccomp: os.Getenv("NOSECCOMP") != ""}
	var log strings.Builder
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	res, err := v.SelfTest(context.Background(), Job{BuildID: "bld_selftest", ProxyAddr: "127.0.0.1 " + port, ProxyCred: user + ":" + pass, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	t.Logf("self-test: %s", res.SelfTest)
	for _, want := range []string{"vm=up", "dists/noble/Release=200", "example.com/=403", "direct=blocked", "mkosi=present"} {
		if !strings.Contains(res.SelfTest, want) {
			t.Errorf("missing %q in %q", want, res.SelfTest)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(denied) != 1 || denied[0] != "example.com" {
		t.Errorf("denied egress: %v", denied)
	}
}
