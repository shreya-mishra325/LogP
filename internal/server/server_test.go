package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"browsercompute.local/runtime/internal/identity"
	"browsercompute.local/runtime/internal/protocol"
	"browsercompute.local/runtime/internal/workload"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const sample = `{"kernel":"mandelbrot.v1","width":17,"height":19,"max_iter":32,"xmin":-2.2,"xmax":0.8,"ymin":-1.2,"ymax":1.2,"tile_size":16}`

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	app, err := New(context.Background(), "operator", "worker", identity.Display{Name: "Alternate Display", CLI: "alternate"})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(app)
	t.Cleanup(func() { app.Close(); s.Close() })
	return app, s.URL
}

func httpCall(t *testing.T, method, address, token, body string, code int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != code {
		t.Fatalf("%s %s got %d: %v %s", method, address, res.StatusCode, err, data)
	}
	return data
}

func dial(t *testing.T, ctx context.Context, url, origin string, want int) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	c, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/ws/v1/worker", &websocket.DialOptions{HTTPHeader: h})
	if want != 101 {
		if res == nil || res.StatusCode != want {
			t.Fatalf("upgrade got %v %v", res, err)
		}
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, typ string, payload any) {
	t.Helper()
	if err := wsjson.Write(ctx, c, protocol.Out{V: 1, Type: typ, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, ctx context.Context, c *websocket.Conn, kind string, dst any) {
	t.Helper()
	var m protocol.Frame
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatal(err)
	}
	if m.V != 1 || m.Type != kind {
		t.Fatalf("want %s got %+v", kind, m)
	}
	if dst != nil {
		if err := json.Unmarshal(m.Payload, dst); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHTTPAndWorkerBoundaries(t *testing.T) {
	app, url := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	for _, page := range []string{"/", "/worker"} {
		if !bytes.Contains(httpCall(t, "GET", url+page, "", "", 200), []byte("Alternate Display")) {
			t.Fatal(page)
		}
	}
	httpCall(t, "GET", url+"/healthz", "", "", 200)
	httpCall(t, "GET", url+"/api/v1/workers", "worker", "", 401)
	httpCall(t, "POST", url+"/api/v1/jobs", "", "", 401)
	for _, bad := range []string{`null`, sample + "{}", strings.Replace(sample, `"width":17`, `"Width":17`, 1), strings.Replace(sample, `"width":17`, `"width":16,"width":17`, 1), strings.Replace(sample, `"xmin":-2.2,`, "", 1), strings.Replace(sample, `"width":17`, `"width":99999`, 1), strings.Repeat(" ", 65537) + sample} {
		httpCall(t, "POST", url+"/api/v1/jobs", "operator", bad, 400)
	}
	dial(t, ctx, url, "", 403)
	dial(t, ctx, url, "https"+strings.TrimPrefix(url, "http"), 403)
	dial(t, ctx, url, "http://example.net", 403)
	c := dial(t, ctx, url, url, 101)
	send(t, ctx, c, "hello", protocol.Hello{Token: "operator", Slots: 1, Kernels: []string{workload.Kernel}})
	receive(t, ctx, c, "error", nil)

	c = dial(t, ctx, url, url, 101)
	send(t, ctx, c, "hello", protocol.Hello{Token: "worker", Slots: 1, Kernels: []string{workload.Kernel}})
	receive(t, ctx, c, "welcome", nil)
	send(t, ctx, c, "ready", map[string]int{"unexpected": 1})
	receive(t, ctx, c, "error", nil)

	c = dial(t, ctx, url, url, 101)
	if err := c.Write(ctx, websocket.MessageText, bytes.Repeat([]byte(" "), 1<<20+1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("excess frame: %v", err)
	}

	c = dial(t, ctx, url, url, 101)
	send(t, ctx, c, "hello", protocol.Hello{Token: "worker", Slots: 1, Kernels: []string{workload.Kernel}})
	receive(t, ctx, c, "welcome", nil)
	app.Close()
	if _, _, err := c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("connection failed to shut down: %v", err)
	}
}

func TestWireAssignmentsAndPNG(t *testing.T) {
	_, url := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c := dial(t, ctx, url, url, 101)
	send(t, ctx, c, "hello", protocol.Hello{Token: "worker", Slots: 1, Kernels: []string{workload.Kernel}})
	receive(t, ctx, c, "welcome", nil)
	var job struct {
		ID    string
		Total int
		Input workload.Input
	}
	if err := json.Unmarshal(httpCall(t, "POST", url+"/api/v1/jobs", "operator", sample, 201), &job); err != nil {
		t.Fatal(err)
	}
	path := url + "/api/v1/jobs/" + job.ID
	httpCall(t, "GET", path+"/result", "operator", "", 409)
	var expected []workload.Tile
	for range job.Total {
		send(t, ctx, c, "ready", map[string]int{"available_slots": 1})
		var a protocol.Assignment
		receive(t, ctx, c, "assign", &a)
		data := base64.StdEncoding.EncodeToString(workload.Reference(a.Params))
		send(t, ctx, c, "result", protocol.Result{Lease: a.Lease, Data: &data})
		var ack protocol.Ack
		receive(t, ctx, c, "result_ack", &ack)
		if ack.Disposition != "accepted" {
			t.Fatal(ack)
		}
		expected = append(expected, workload.Tile{Rect: a.Params.Rect, Data: workload.Reference(a.Params)})
	}
	got := httpCall(t, "GET", path+"/result", "operator", "", 200)
	if _, err := png.Decode(bytes.NewReader(got)); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := workload.PNG(&want, job.Input, expected); err != nil || !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("incorrect PNG: %v", err)
	}
}
