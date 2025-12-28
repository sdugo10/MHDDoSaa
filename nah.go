package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

type atomicCounter struct {
	v uint64
}

func (c *atomicCounter) Inc() {
	atomic.AddUint64(&c.v, 1)
}

func (c *atomicCounter) Get() uint64 {
	return atomic.LoadUint64(&c.v)
}

type stats struct {
	mu        sync.Mutex
	latencies []float64
	success   uint64
	errors    uint64
}

type ConnectionManager struct {
	targetURL string
	tlsCfg    *tls.Config
	quicCfg   *quic.Config
	clients   []*http.Client // Changed from http3.RoundTripper to http.Client
	mu        sync.Mutex
	shutdown  bool
}

func (cm *ConnectionManager) Init(num int) error {
	var wg sync.WaitGroup
	failed := 0

	for i := 0; i < num; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			client, err := cm.createHTTP3Client()
			if err != nil {
				failed++
				log.Printf("Init connection %d failed: %v", idx+1, err)
				return
			}
			cm.mu.Lock()
			cm.clients = append(cm.clients, client)
			cm.mu.Unlock()
			fmt.Printf(" Đã tạo kết nối %d/%d...\n", idx+1, num)
		}(i)
	}
	wg.Wait()

	if len(cm.clients) == 0 {
		return fmt.Errorf("không thể tạo bất kỳ kết nối nào (%d thất bại)", failed)
	}
	fmt.Printf("✓ Đã tạo thành công %d/%d kết nối\n", len(cm.clients), num)
	return nil
}

func (cm *ConnectionManager) createHTTP3Client() (*http.Client, error) {
	// Create HTTP/3 client with custom transport
	transport := &http3.Transport{
		TLSClientConfig: cm.tlsCfg,
		QUICConfig:      cm.quicCfg,
		EnableDatagrams: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", cm.targetURL, nil)
	if err != nil {
		transport.Close()
		return nil, err
	}
	addChromeHeaders(req)

	_, err = client.Do(req)
	if err != nil {
		transport.Close()
		return nil, err
	}
	return client, nil
}

func (cm *ConnectionManager) Get() *http.Client {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.shutdown || len(cm.clients) == 0 {
		return nil
	}
	return cm.clients[rand.Intn(len(cm.clients))]
}

func (cm *ConnectionManager) Recover(old *http.Client) *http.Client {
	if old == nil {
		return nil
	}

	cm.mu.Lock()
	if cm.shutdown {
		cm.mu.Unlock()
		return nil
	}
	// Remove old client
	for i, c := range cm.clients {
		if c == old {
			cm.clients = append(cm.clients[:i], cm.clients[i+1:]...)
			break
		}
	}
	cm.mu.Unlock()

	// Close the transport of old client
	if transport, ok := old.Transport.(*http3.Transport); ok {
		transport.Close()
	}

	// Create new client
	newClient, err := cm.createHTTP3Client()
	if err != nil {
		log.Printf("Recovery failed: %v", err)
		return nil
	}

	cm.mu.Lock()
	cm.clients = append(cm.clients, newClient)
	cm.mu.Unlock()

	log.Println("Kết nối đã được phục hồi thành công")
	return newClient
}

func (cm *ConnectionManager) CloseAll() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.shutdown = true
	for _, client := range cm.clients {
		if transport, ok := client.Transport.(*http3.Transport); ok {
			transport.Close()
		}
	}
	cm.clients = nil
}

func addChromeHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Sec-Ch-Ua", `"Not)A;Brand";v="8", "Chromium";v="131", "Google Chrome";v="131"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Priority", "u=0, i")
	req.Header.Set("Sec-Ch-Prefers-Color-Scheme", "light")
	req.Header.Set("Viewport-Width", "1920")
}

func main() {
	var (
		targetURL      string
		targetRPS      int
		workers        int
		connections    int
		duration       int
		maxConcurrency int
	)

	flag.StringVar(&targetURL, "url", "", "Target URL (HTTPS only)")
	flag.IntVar(&targetRPS, "rps", 0, "Target requests per second")
	flag.IntVar(&workers, "workers", 300, "Number of worker goroutines")
	flag.IntVar(&connections, "connections", 80, "Initial QUIC connections")
	flag.IntVar(&duration, "duration", 30, "Test duration in seconds")
	flag.IntVar(&maxConcurrency, "max-concurrency", 1500, "Max concurrent requests")
	flag.Parse()

	if targetURL == "" || targetRPS == 0 {
		flag.Usage()
		os.Exit(1)
	}

	u, err := url.Parse(targetURL)
	if err != nil || u.Scheme != "https" {
		log.Fatal("URL phải là HTTPS hợp lệ")
	}

	fmt.Printf("Bắt đầu load test HTTP/3 → %s\n", targetURL)
	fmt.Printf("Mục tiêu: %d RPS | Thời gian: %ds | Workers: %d | Connections: %d | Concurrency: %d\n",
		targetRPS, duration, workers, connections, maxConcurrency)

	cm := &ConnectionManager{targetURL: targetURL}
	cm.tlsCfg = &tls.Config{
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		NextProtos:       []string{"h3"},
	}

	cm.quicCfg = &quic.Config{
		MaxIncomingStreams:         1500,
		MaxIncomingUniStreams:      1500,
		MaxStreamReceiveWindow:     6 * 1024 * 1024,
		MaxConnectionReceiveWindow: 15 * 1024 * 1024,
		KeepAlivePeriod:            15 * time.Second,
		Allow0RTT:                  true,
		EnableDatagrams:            true,
	}

	if err := cm.Init(connections); err != nil {
		log.Fatal(err)
	}
	defer cm.CloseAll()

	counter := &atomicCounter{}
	st := &stats{}

	sem := make(chan struct{}, maxConcurrency)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	startTime := time.Now()
	endTime := startTime.Add(time.Duration(duration) * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for time.Now().Before(endTime) && ctx.Err() == nil {
				select {
				case sem <- struct{}{}:
				default:
					time.Sleep(1 * time.Millisecond)
					continue
				}

				client := cm.Get()
				if client == nil {
					<-sem
					continue
				}

				rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
				req, err := http.NewRequestWithContext(rctx, "GET", targetURL, nil)
				if err != nil {
					atomic.AddUint64(&st.errors, 1)
					<-sem
					rcancel()
					continue
				}
				addChromeHeaders(req)

				startReq := time.Now()
				resp, err := client.Do(req) // Changed from rt.RoundTrip to client.Do
				latency := time.Since(startReq).Seconds() * 1000
				rcancel()

				if err != nil {
					atomic.AddUint64(&st.errors, 1)
					_ = cm.Recover(client) // recover connection
					if atomic.LoadUint64(&st.errors)%100 == 1 {
						fmt.Printf("Worker %d lỗi (tổng %d): %v\n", id, st.errors, err)
					}
				} else {
					if resp.StatusCode == 200 {
						atomic.AddUint64(&st.success, 1)
					} else {
						atomic.AddUint64(&st.errors, 1)
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}

				counter.Inc()
				if counter.Get()%10 == 0 {
					st.mu.Lock()
					st.latencies = append(st.latencies, latency)
					st.mu.Unlock()
				}
				<-sem
			}
		}(i)
	}

	// Monitor
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		lastCount := uint64(0)
		lastTime := startTime
		for range ticker.C {
			if ctx.Err() != nil {
				break
			}
			curr := counter.Get()
			now := time.Now()
			elapsed := now.Sub(lastTime).Seconds()
			rps := 0.0
			if elapsed > 0 {
				rps = float64(curr-lastCount) / elapsed
			}
			fmt.Printf("Tiến độ: %d req | RPS hiện tại: %.1f | Lỗi: %d\n", curr, rps, atomic.LoadUint64(&st.errors))
			lastCount = curr
			lastTime = now
		}
	}()

	wg.Wait()

	totalTime := time.Since(startTime).Seconds()
	totalReq := counter.Get()
	avgRPS := float64(totalReq) / totalTime

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("LOAD TEST HOÀN THÀNH")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Thời gian chạy:     %.2f giây\n", totalTime)
	fmt.Printf("Tổng request:       %d\n", totalReq)
	fmt.Printf("RPS trung bình:     %.2f\n", avgRPS)
	fmt.Printf("Mục tiêu RPS:       %d\n", targetRPS)
	fmt.Printf("Hiệu quả:           %.1f%%\n", (avgRPS/float64(targetRPS))*100)
	fmt.Printf("Thành công:         %d\n", atomic.LoadUint64(&st.success))
	fmt.Printf("Lỗi:                %d\n", atomic.LoadUint64(&st.errors))

	st.mu.Lock()
	latencies := st.latencies
	st.mu.Unlock()
	if len(latencies) > 0 {
		sort.Float64s(latencies)
		minLat := latencies[0]
		maxLat := latencies[len(latencies)-1]
		sum := 0.0
		for _, v := range latencies {
			sum += v
		}
		avgLat := sum / float64(len(latencies))
		p95Idx := int(math.Floor(0.95 * float64(len(latencies))))
		p95 := latencies[p95Idx]
		fmt.Printf("Latency (ms): Min=%.1f  Avg=%.1f  Max=%.1f  P95=%.1f\n", minLat, avgLat, maxLat, p95)
	}

	fmt.Printf("Workers hoàn thành: %d/%d\n", workers, workers)
}