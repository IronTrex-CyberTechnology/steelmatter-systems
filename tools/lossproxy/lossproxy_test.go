package lossproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upstream is a fake Odoo that counts the steelmatter.api calls it applied.
func upstream(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	var applied atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		applied.Add(1)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"outcome":"CREATED","op_ref":"op-1"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &applied
}

func rpcBody(method string, arg any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "call", "params": map[string]any{
		"service": "object", "method": "execute_kw", "args": []any{"smr1", 6, "pw", "steelmatter.api", method, []any{arg}, map[string]any{}}}})
	return b
}

func setup(t *testing.T, delay time.Duration) (*Proxy, *httptest.Server, *atomic.Int64) {
	up, applied := upstream(t, delay)
	p, err := New(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return p, front, applied
}

func post(front *httptest.Server, body []byte, timeout time.Duration) (*http.Response, error) {
	c := &http.Client{Timeout: timeout}
	return c.Post(front.URL+"/jsonrpc", "application/json", bytes.NewReader(body))
}

func TestPassThrough(t *testing.T) {
	p, front, applied := setup(t, 0)
	resp, err := post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-1"}), time.Second)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "CREATED") || applied.Load() != 1 {
		t.Fatalf("body %s, applied %d", b, applied.Load())
	}
	ev := p.Events("op-1")
	if len(ev) != 1 || ev[0].Action != "forwarded" || ev[0].Method != "sm_create" || ev[0].UpstreamOutcome != "CREATED" {
		t.Fatalf("events %+v", ev)
	}
}

func TestDropReplyAppliesUpstreamButClientGetsNothing(t *testing.T) {
	p, front, applied := setup(t, 0)
	if err := p.Add(Fault{Mode: DropReply, Method: "sm_create", OpRef: "op-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-1"}), time.Second); err == nil {
		t.Fatal("the client got a reply")
	}
	if applied.Load() != 1 {
		t.Fatalf("upstream applied %d", applied.Load())
	}
	ev := p.Events("op-1")
	if len(ev) != 1 || ev[0].Action != "drop_reply" || !ev[0].Forwarded || ev[0].UpstreamOutcome != "CREATED" {
		t.Fatalf("events %+v", ev)
	}
	// One use only: the next call passes.
	if resp, err := post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-1"}), time.Second); err != nil || resp.StatusCode != 200 {
		t.Fatalf("second call %v %v", resp, err)
	}
}

func TestDropRequestNeverReachesUpstream(t *testing.T) {
	p, front, applied := setup(t, 0)
	_ = p.Add(Fault{Mode: DropRequest, Method: "sm_fence"})
	if _, err := post(front, rpcBody("sm_fence", "op-2"), time.Second); err == nil {
		t.Fatal("the client got a reply")
	}
	if applied.Load() != 0 {
		t.Fatal("the request reached upstream")
	}
	if ev := p.Events("op-2"); len(ev) != 1 || ev[0].Forwarded || ev[0].Action != "drop_request" {
		t.Fatalf("events %+v", ev)
	}
}

func TestDelayReplyPastClientTimeoutStillAppliesUpstream(t *testing.T) {
	p, front, applied := setup(t, 0)
	_ = p.Add(Fault{Mode: DelayReply, Method: "sm_create", DelayMS: 500})
	if _, err := post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-3"}), 100*time.Millisecond); err == nil {
		t.Fatal("expected a client timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for applied.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if applied.Load() != 1 {
		t.Fatal("upstream did not apply the delayed call")
	}
}

func TestBadGateway(t *testing.T) {
	p, front, applied := setup(t, 0)
	_ = p.Add(Fault{Mode: BadGateway, Method: "sm_create", Count: 2})
	for i := 0; i < 2; i++ {
		resp, err := post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-4"}), time.Second)
		if err != nil || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("call %d: %v %v", i, resp, err)
		}
	}
	if applied.Load() != 2 || len(p.Pending()) != 0 {
		t.Fatalf("applied %d pending %v", applied.Load(), p.Pending())
	}
}

func TestFaultMatchesMethodAndOpRef(t *testing.T) {
	p, front, _ := setup(t, 0)
	_ = p.Add(Fault{Mode: DropReply, Method: "sm_create", OpRef: "op-x"})
	for _, body := range [][]byte{rpcBody("sm_create", map[string]any{"op_ref": "op-other"}), rpcBody("sm_lookup", "op-x")} {
		if resp, err := post(front, body, time.Second); err != nil || resp.StatusCode != 200 {
			t.Fatalf("non-matching call was faulted: %v %v", resp, err)
		}
	}
	if len(p.Pending()) != 1 {
		t.Fatal("the fault was used by a non-matching call")
	}
}

func TestControlAPI(t *testing.T) {
	p, front, _ := setup(t, 0)
	ctl := httptest.NewServer(p.Control())
	t.Cleanup(ctl.Close)
	resp, err := http.Post(ctl.URL+"/faults", "application/json", strings.NewReader(`{"mode":"drop_reply","method":"sm_create","op_ref":"op-5"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	if bad, _ := http.Post(ctl.URL+"/faults", "application/json", strings.NewReader(`{"mode":"explode","method":"x"}`)); bad.StatusCode != 400 {
		t.Fatalf("unknown mode accepted: %d", bad.StatusCode)
	}
	_, _ = post(front, rpcBody("sm_create", map[string]any{"op_ref": "op-5"}), time.Second)
	r, _ := http.Get(ctl.URL + "/events?op_ref=op-5")
	var ev []Event
	_ = json.NewDecoder(r.Body).Decode(&ev)
	if len(ev) != 1 || ev[0].Action != "drop_reply" {
		t.Fatalf("events %+v", ev)
	}
	req, _ := http.NewRequest(http.MethodDelete, ctl.URL+"/faults", nil)
	if d, _ := http.DefaultClient.Do(req); d.StatusCode != http.StatusNoContent || len(p.Events("")) != 0 {
		t.Fatal("reset failed")
	}
}
