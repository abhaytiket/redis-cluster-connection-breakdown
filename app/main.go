// Command redis-app is a small load generator that talks to a Redis Cluster
// through go-redis' pooled ClusterClient. Every pooled connection is tagged
// with CLIENT_NAME (via CLIENT SETNAME / HELLO SETNAME) so the connections can
// be broken down per application on the Redis side.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

type config struct {
	clientName   string
	addrs        []string
	poolSize     int
	minIdleConns int
	workers      int
	rampInterval time.Duration
	opDelay      time.Duration
	keySpace     int
}

func loadConfig() config {
	return config{
		clientName:   envString("CLIENT_NAME", "redis-app"),
		addrs:        strings.Split(envString("REDIS_ADDRS", "redis-node-1:6379,redis-node-2:6379,redis-node-3:6379"), ","),
		poolSize:     envInt("POOL_SIZE", 20),
		minIdleConns: envInt("MIN_IDLE_CONNS", 5),
		workers:      envInt("WORKERS", 30),
		rampInterval: envDuration("RAMP_INTERVAL", 0),
		opDelay:      envDuration("OP_DELAY", 20*time.Millisecond),
		keySpace:     envInt("KEY_SPACE", 10000),
	}
}

func main() {
	cfg := loadConfig()
	log.SetPrefix(fmt.Sprintf("[%s] ", cfg.clientName))
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:         cfg.addrs,
		ClientName:    cfg.clientName,
		PoolSize:      cfg.poolSize,     // max connections per cluster node
		MinIdleConns:  cfg.minIdleConns, // warm connections kept per cluster node
		ReadOnly:      true,             // allow reads from replicas
		RouteRandomly: true,             // spread reads across primary + replicas
	})
	defer client.Close()

	if err := waitForCluster(ctx, client); err != nil {
		log.Fatalf("cluster not reachable: %v", err)
	}
	log.Printf("connected: pool_size=%d min_idle_conns=%d workers=%d ramp_interval=%s",
		cfg.poolSize, cfg.minIdleConns, cfg.workers, cfg.rampInterval)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportPoolStats(ctx, client)
	}()

	for i := 0; i < cfg.workers; i++ {
		if i > 0 && cfg.rampInterval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(cfg.rampInterval):
				log.Printf("ramp: started worker %d/%d", i+1, cfg.workers)
			}
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			worker(ctx, client, cfg, id)
		}(i)
	}

	<-ctx.Done()
	log.Printf("shutting down")
	wg.Wait()
}

func waitForCluster(ctx context.Context, client *redis.ClusterClient) error {
	var err error
	for attempt := 0; attempt < 60; attempt++ {
		if err = client.Ping(ctx).Err(); err == nil {
			return nil
		}
		log.Printf("waiting for cluster: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

func worker(ctx context.Context, client *redis.ClusterClient, cfg config, id int) {
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(id)))
	for ctx.Err() == nil {
		key := fmt.Sprintf("%s:key:%d", cfg.clientName, r.IntN(cfg.keySpace))

		var err error
		switch n := r.IntN(100); {
		case n < 40:
			err = client.Set(ctx, key, strconv.Itoa(r.Int()), time.Minute).Err()
		case n < 80:
			err = client.Get(ctx, key).Err()
		case n < 90:
			err = client.Incr(ctx, key+":counter").Err()
		default:
			// Blocking pop on an (almost always) empty list holds a pooled
			// connection for a while, which forces the pool to grow under load.
			// Sent via Do because client.BLPop rounds sub-second timeouts up to 1s.
			err = client.Do(ctx, "BLPOP", key+":queue", "0.2").Err()
		}
		if err != nil && !errors.Is(err, redis.Nil) && ctx.Err() == nil {
			log.Printf("worker %d: %v", id, err)
			time.Sleep(500 * time.Millisecond)
		}

		select {
		case <-ctx.Done():
		case <-time.After(cfg.opDelay):
		}
	}
}

func reportPoolStats(ctx context.Context, client *redis.ClusterClient) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s := client.PoolStats()
		log.Printf("pool total: conns=%d idle=%d stale=%d hits=%d misses=%d timeouts=%d",
			s.TotalConns, s.IdleConns, s.StaleConns, s.Hits, s.Misses, s.Timeouts)

		var mu sync.Mutex
		var lines []string
		collect := func(_ context.Context, node *redis.Client) error {
			ns := node.PoolStats()
			mu.Lock()
			lines = append(lines, fmt.Sprintf("%s conns=%d idle=%d", node.Options().Addr, ns.TotalConns, ns.IdleConns))
			mu.Unlock()
			return nil
		}
		_ = client.ForEachShard(ctx, collect)
		if len(lines) > 0 {
			log.Printf("pool per node: %s", strings.Join(lines, " | "))
		}
	}
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("invalid %s=%q: %v", key, v, err)
		}
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid %s=%q: %v", key, v, err)
		}
		return d
	}
	return def
}
