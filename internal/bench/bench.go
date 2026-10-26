// Package bench measures the host side of verification: disk throughput
// where VM disks live, internet bandwidth, and same-site host-to-host
// bandwidth. GPU checks run inside a throwaway VM (see vm.BenchScript).
package bench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/runcrateai/runcrate-host/internal/api"
)

// Disk writes and reads a 2 GiB file under dir with fsync and O_DIRECT where
// the filesystem allows, and removes it.
func Disk(dir string) api.DiskBench {
	const size = 2 << 30
	p := filepath.Join(dir, ".runcrate-diskbench")
	defer os.Remove(p)
	buf := aligned(8<<20, 4096)
	_, _ = rand.Read(buf)
	f, err := openDirect(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return api.DiskBench{Error: err.Error()}
	}
	start := time.Now()
	for w := 0; w < size; w += len(buf) {
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return api.DiskBench{Error: err.Error()}
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return api.DiskBench{Error: err.Error()}
	}
	f.Close()
	wsec := time.Since(start).Seconds()
	f, err = openDirect(p, os.O_RDONLY)
	if err != nil {
		return api.DiskBench{Error: err.Error()}
	}
	defer f.Close()
	start = time.Now()
	var n int64
	for {
		k, err := f.Read(buf)
		n += int64(k)
		if err == io.EOF {
			break
		}
		if err != nil {
			return api.DiskBench{Error: err.Error()}
		}
	}
	rsec := time.Since(start).Seconds()
	return api.DiskBench{
		WriteMbps: round(float64(size) * 8 / 1e6 / wsec),
		ReadMbps:  round(float64(n) * 8 / 1e6 / rsec),
	}
}

// aligned returns a size-byte slice whose start is a multiple of align
// (O_DIRECT needs it).
func aligned(size, align int) []byte {
	b := make([]byte, size+align)
	off := int(uintptr(unsafe.Pointer(&b[0])) & uintptr(align-1))
	if off != 0 {
		off = align - off
	}
	return b[off : off+size]
}

func round(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// SpeedTest is where internet bandwidth is measured (outbound HTTPS only).
type SpeedTest struct {
	DownURL string // GET, returns at least the bytes asked for
	UpURL   string // POST sink
	HTTP    *http.Client
}

// DefaultSpeedTest uses Cloudflare's public speed test endpoints.
var DefaultSpeedTest = SpeedTest{
	DownURL: "https://speed.cloudflare.com/__down?bytes=200000000",
	UpURL:   "https://speed.cloudflare.com/__up",
}

// Net measures download and upload over a few parallel streams.
func (s SpeedTest) Net(ctx context.Context) api.NetBench {
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 90 * time.Second}
	}
	var out api.NetBench
	const streams = 4
	down, err := parallel(streams, func() (int64, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.DownURL, nil)
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("download: HTTP %d", resp.StatusCode)
		}
		return io.Copy(io.Discard, resp.Body)
	})
	if err != nil {
		out.Error = err.Error()
	}
	out.DownMbps = round(down)
	payload := make([]byte, 25<<20)
	up, err := parallel(streams, func() (int64, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.UpURL, bytes.NewReader(payload))
		req.ContentLength = int64(len(payload))
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode/100 != 2 {
			return 0, fmt.Errorf("upload: HTTP %d", resp.StatusCode)
		}
		return int64(len(payload)), nil
	})
	if err != nil && out.Error == "" {
		out.Error = err.Error()
	}
	out.UpMbps = round(up)
	return out
}

// parallel runs fn n times at once and returns the aggregate Mbit/s.
func parallel(n int, fn func() (int64, error)) (float64, error) {
	type res struct {
		b   int64
		err error
	}
	ch := make(chan res, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() { b, err := fn(); ch <- res{b, err} }()
	}
	var total int64
	var errs []error
	for i := 0; i < n; i++ {
		r := <-ch
		total += r.b
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	sec := time.Since(start).Seconds()
	if total == 0 {
		return 0, errors.Join(errs...)
	}
	return float64(total) * 8 / 1e6 / sec, nil
}

// PeerWindow is how long a host serves iperf3 during its benchmark.
const PeerWindow = 5 * time.Minute

// ibPort derives a per-pair port, so a host with several lower peers can
// serve each one its own ib_write_bw.
func ibPort(server, client string) int {
	h := fnv.New32a()
	h.Write([]byte(server + "|" + client))
	return 18600 + int(h.Sum32()%400)
}

// Peers measures bandwidth to same-site hosts. Every host serves iperf3 on
// its private addresses for PeerWindow; the host with the lower id dials each
// higher one, so each pair is measured once. Missing tools or a closed
// private network give a result with an error, never a failure.
func Peers(ctx context.Context, self string, myAddrs []string, peers []api.Peer, rdma bool) []api.PeerResult {
	if len(peers) == 0 {
		return nil
	}
	iperf, err := exec.LookPath("iperf3")
	if err != nil {
		var out []api.PeerResult
		for _, p := range peers {
			out = append(out, api.PeerResult{HostID: p.HostID, Error: "iperf3 not installed"})
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, PeerWindow)
	defer cancel()
	var servers []*exec.Cmd
	for _, a := range myAddrs {
		cmd := exec.CommandContext(ctx, iperf, "-s", "-B", a, "-p", "5201")
		if cmd.Start() == nil {
			servers = append(servers, cmd)
		}
	}
	if rdma {
		for _, p := range peers {
			if p.HostID < self {
				cmd := exec.CommandContext(ctx, "ib_write_bw", "-p", strconv.Itoa(ibPort(self, p.HostID)), "-F", "--report_gbits", "-D", "5")
				if cmd.Start() == nil {
					servers = append(servers, cmd)
				}
			}
		}
	}
	var out []api.PeerResult
	for _, p := range peers {
		if p.HostID <= self {
			continue
		}
		out = append(out, measure(ctx, iperf, self, p, rdma))
	}
	// Stay up for the higher-id peers that dial us.
	if hasLower(self, peers) {
		<-ctx.Done()
	}
	for _, s := range servers {
		_ = s.Process.Kill()
		_ = s.Wait()
	}
	return out
}

func hasLower(self string, peers []api.Peer) bool {
	for _, p := range peers {
		if p.HostID < self {
			return true
		}
	}
	return false
}

func measure(ctx context.Context, iperf, self string, p api.Peer, rdma bool) api.PeerResult {
	r := api.PeerResult{HostID: p.HostID}
	for _, a := range p.PrivateAddrs {
		if net.ParseIP(a) == nil {
			continue
		}
		// Retry while the peer's server comes up.
		var lastErr error
		for i := 0; i < 12 && ctx.Err() == nil; i++ {
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(a, "5201"))
			if err != nil {
				lastErr = err
				time.Sleep(10 * time.Second)
				continue
			}
			conn.Close()
			lastErr = nil
			break
		}
		if lastErr != nil {
			r.Error = "no private route: " + lastErr.Error()
			continue
		}
		r.Addr, r.Reachable, r.Error = a, true, ""
		out, err := exec.CommandContext(ctx, iperf, "-c", a, "-p", "5201", "-P", "8", "-t", "10", "-J").Output()
		if err != nil {
			r.Error = "iperf3: " + err.Error()
			return r
		}
		var j struct {
			End struct {
				SumReceived struct {
					BitsPerSecond float64 `json:"bits_per_second"`
				} `json:"sum_received"`
			} `json:"end"`
		}
		if json.Unmarshal(out, &j) == nil {
			r.IperfGbps = round(j.End.SumReceived.BitsPerSecond / 1e9)
		}
		if rdma {
			if v, err := ibWriteBW(ctx, a, ibPort(p.HostID, self)); err == nil {
				r.IBWriteBWGbps = &v
			}
		}
		return r
	}
	return r
}

func ibWriteBW(ctx context.Context, addr string, port int) (float64, error) {
	var last error
	for i := 0; i < 6; i++ {
		out, err := exec.CommandContext(ctx, "ib_write_bw", "-p", strconv.Itoa(port), "-F", "--report_gbits", "-D", "5", addr).Output()
		if err == nil {
			// The last numeric line: "#bytes #iterations BW peak BW average ..."
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				f := strings.Fields(lines[i])
				if len(f) >= 4 {
					if v, err := strconv.ParseFloat(f[3], 64); err == nil {
						return round(v), nil
					}
				}
			}
			return 0, errors.New("ib_write_bw: no result")
		}
		last = err
		time.Sleep(5 * time.Second)
	}
	return 0, last
}
