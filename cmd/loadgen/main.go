// Command loadgen is an open-model evaluation load generator for the local benchmark stack.
//
// It exists because k6 (JavaScript VUs) saturates the 4 GiB Docker VM used for local runs before
// the API does. Like k6's arrival-rate executors it offers load on a schedule regardless of
// response time; iterations that cannot start because too many are in flight are counted as
// dropped, never silently delayed. Every response is checked against the fixture's expected
// decision, so throughput is only reported alongside correctness.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	pb "switchyard/pkg/api/switchyard/v1"
)

type fixtureFlag struct {
	Key      string          `json:"key"`
	Kind     string          `json:"kind"`
	RunID    string          `json:"run_id"`
	Expected json.RawMessage `json:"expected"`
	Safe     *struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"safe"`
}
type fixture struct {
	ProjectID     string        `json:"project_id"`
	EnvironmentID string        `json:"environment_id"`
	Token         string        `json:"token"`
	Flags         []fixtureFlag `json:"flags"`
}

type decision struct {
	Value struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"value"`
	Reason     string `json:"reason"`
	RunID      string `json:"run_id"`
	VariantID  string `json:"variant_id"`
	DecisionID string `json:"decision_id"`
}

var countries = []string{"JP", "US", "FR", "BR"}

func equalJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// correct applies the same expectations as the k6 workload.
func correct(f fixtureFlag, d decision, country string, age int) bool {
	if d.DecisionID == "" {
		return false
	}
	isTrue := string(d.Value.Data) == "true"
	isFalse := string(d.Value.Data) == "false"
	switch f.Kind {
	case "always_on":
		return isTrue && d.Reason == "default"
	case "rules_country":
		if country == "JP" {
			return isTrue && d.Reason == "targeting"
		}
		return isFalse && d.Reason == "default"
	case "rules_age":
		if age >= 18 {
			return isTrue && d.Reason == "targeting"
		}
		return isFalse && d.Reason == "default"
	case "json":
		return d.Value.Type == "json" && equalJSON(d.Value.Data, f.Expected)
	case "killed":
		return isFalse && d.Reason == "kill_switch"
	case "rollout":
		if d.Reason == "rollout" {
			return isTrue
		}
		return d.Reason == "default" && isFalse
	case "experiment":
		if d.Reason != "experiment" || d.RunID != f.RunID {
			return false
		}
		return (d.VariantID == "control" && isFalse) || (d.VariantID == "treatment" && isTrue)
	}
	return false
}

type result struct {
	Target     int       `json:"target_requests_per_second"`
	Transport  string    `json:"transport"`
	Seconds    float64   `json:"seconds"`
	Completed  int64     `json:"requests"`
	Achieved   float64   `json:"achieved_requests_per_second_during_hold"`
	Failures   int64     `json:"failures"`
	Incorrect  int64     `json:"incorrect"`
	Dropped    int64     `json:"dropped_iterations"`
	MaxInFly   int64     `json:"max_in_flight"`
	LatencyMS  latencyMS `json:"latency_ms"`
	FailureRte float64   `json:"failure_rate"`
	Correct    float64   `json:"correctness_rate"`
}
type latencyMS struct {
	Avg float64 `json:"avg"`
	Med float64 `json:"med"`
	P90 float64 `json:"p(90)"`
	P95 float64 `json:"p(95)"`
	P99 float64 `json:"p(99)"`
	Max float64 `json:"max"`
}

type runner struct {
	fx        fixture
	transport string
	client    *http.Client
	baseURL   string
	grpc      pb.EvaluationServiceClient
	authCtx   context.Context
}

func (r *runner) one(n int64) (latency time.Duration, failed, wrong bool) {
	f := r.fx.Flags[int(n)%len(r.fx.Flags)]
	user := "eval-user-" + strconv.FormatInt((n*7919)%100000, 10)
	country := countries[(n>>3)%int64(len(countries))]
	age := 10 + int((n>>5)%50)
	safeType, safe := "boolean", json.RawMessage("false")
	if f.Safe != nil {
		safeType, safe = f.Safe.Type, f.Safe.Data
	}
	attributes := fmt.Sprintf(`{"country":%q,"age":%d,"tier":"standard"}`, country, age)
	var d decision
	started := time.Now()
	switch r.transport {
	case "http":
		body := fmt.Sprintf(`{"project_id":%q,"environment_id":%q,"key":%q,"user_id":%q,"attributes":%s,"fallback":{"type":%q,"data":%s}}`,
			r.fx.ProjectID, r.fx.EnvironmentID, f.Key, user, attributes, safeType, safe)
		request, _ := http.NewRequest(http.MethodPost, r.baseURL+"/v1/evaluate", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+r.fx.Token)
		request.Header.Set("Content-Type", "application/json")
		response, err := r.client.Do(request)
		if err != nil {
			return time.Since(started), true, false
		}
		payload, err := io.ReadAll(response.Body)
		response.Body.Close()
		latency = time.Since(started)
		if err != nil || response.StatusCode != 200 || json.Unmarshal(payload, &d) != nil {
			return latency, true, false
		}
	default:
		response, err := r.grpc.Evaluate(r.authCtx, &pb.EvaluateRequest{ProjectId: r.fx.ProjectID, EnvironmentId: r.fx.EnvironmentID, Input: &pb.Evaluation{
			Key: f.Key, UserId: user, AttributesJson: map[string][]byte{"country": []byte(strconv.Quote(country)), "age": []byte(strconv.Itoa(age)), "tier": []byte(`"standard"`)},
			Fallback: &pb.Value{Type: safeType, Json: safe}}})
		latency = time.Since(started)
		if err != nil || response.Value == nil {
			return latency, true, false
		}
		d.Value.Type, d.Value.Data = response.Value.Type, response.Value.Json
		d.Reason, d.RunID, d.VariantID, d.DecisionID = response.Reason, response.RunId, response.VariantId, response.DecisionId
	}
	return latency, false, !correct(f, d, country, age)
}

func percentile(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*q+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return float64(sorted[i].Microseconds()) / 1000
}

// step offers rate requests/s: a linear ramp from the previous rate, then a hold.
func (r *runner) step(previous, rate int, ramp, hold time.Duration, maxInFlight int64, counter *atomic.Int64) result {
	var inflight, peak, completed, failures, incorrect, dropped atomic.Int64
	var latencies [64][]time.Duration
	var shard [64]sync.Mutex
	var wg sync.WaitGroup
	total := ramp + hold
	start := time.Now()
	issued := int64(0)
	var holdStarted time.Time
	var holdCompletedAtStart int64
	holdSeen := false
	for {
		elapsed := time.Since(start)
		if elapsed >= total {
			break
		}
		var due float64
		if elapsed < ramp {
			t := elapsed.Seconds()
			due = float64(previous)*t + float64(rate-previous)*t*t/(2*ramp.Seconds())
		} else {
			t := (elapsed - ramp).Seconds()
			due = float64(previous)*ramp.Seconds() + float64(rate-previous)*ramp.Seconds()/2 + float64(rate)*t
			if !holdSeen {
				holdSeen, holdStarted, holdCompletedAtStart = true, time.Now(), completed.Load()
			}
		}
		for issued < int64(due) {
			issued++
			if inflight.Load() >= maxInFlight {
				dropped.Add(1)
				continue
			}
			n := counter.Add(1)
			now := inflight.Add(1)
			for {
				p := peak.Load()
				if now <= p || peak.CompareAndSwap(p, now) {
					break
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				latency, failed, wrong := r.one(n)
				inflight.Add(-1)
				completed.Add(1)
				if failed {
					failures.Add(1)
					return
				}
				if wrong {
					incorrect.Add(1)
				}
				s := n & 63
				shard[s].Lock()
				latencies[s] = append(latencies[s], latency)
				shard[s].Unlock()
			}()
		}
		time.Sleep(200 * time.Microsecond)
	}
	holdSeconds := time.Since(holdStarted).Seconds()
	completedDuringHold := completed.Load() - holdCompletedAtStart
	wg.Wait()
	var all []time.Duration
	for i := range latencies {
		all = append(all, latencies[i]...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	var sum time.Duration
	for _, l := range all {
		sum += l
	}
	out := result{Target: rate, Transport: r.transport, Seconds: time.Since(start).Seconds(), Completed: completed.Load(), Failures: failures.Load(), Incorrect: incorrect.Load(),
		Dropped: dropped.Load(), MaxInFly: peak.Load()}
	if holdSeconds > 0 {
		out.Achieved = float64(completedDuringHold) / holdSeconds
	}
	if len(all) > 0 {
		out.LatencyMS = latencyMS{Avg: float64(sum.Microseconds()) / 1000 / float64(len(all)), Med: percentile(all, .5), P90: percentile(all, .9), P95: percentile(all, .95), P99: percentile(all, .99), Max: percentile(all, 1)}
	}
	if out.Completed > 0 {
		out.FailureRte = float64(out.Failures) / float64(out.Completed)
		out.Correct = 1 - float64(out.Incorrect)/float64(max(1, out.Completed-out.Failures))
	}
	return out
}

func main() {
	fixturePath := flag.String("fixture", "/loadtest/fixture.json", "fixture file")
	transport := flag.String("transport", "http", "http or grpc")
	baseURL := flag.String("base", "http://api:8080", "HTTP base URL")
	grpcTarget := flag.String("grpc", "api:9090", "gRPC target")
	rates := flag.String("rates", "500", "comma-separated target requests/s, one step each")
	hold := flag.Duration("hold", 45*time.Second, "hold per step")
	ramp := flag.Duration("ramp", 10*time.Second, "ramp per step")
	warmup := flag.Duration("warmup", 20*time.Second, "warmup at 200/s before the first step")
	maxInFlight := flag.Int64("max-in-flight", 4000, "iterations that cannot start beyond this are dropped")
	outPath := flag.String("out", "", "write the JSON summary here")
	flag.Parse()
	raw, err := os.ReadFile(*fixturePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	r := &runner{transport: *transport, baseURL: *baseURL}
	if err := json.Unmarshal(raw, &r.fx); err != nil || len(r.fx.Flags) == 0 {
		fmt.Fprintln(os.Stderr, "invalid fixture")
		os.Exit(2)
	}
	switch *transport {
	case "http":
		r.client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConns: 8192, MaxIdleConnsPerHost: 8192, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second, DisableCompression: true}}
	case "grpc":
		conn, err := grpc.NewClient(*grpcTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer conn.Close()
		r.grpc = pb.NewEvaluationServiceClient(conn)
		callCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.authCtx = metadata.AppendToOutgoingContext(callCtx, "authorization", "Bearer "+r.fx.Token)
	default:
		fmt.Fprintln(os.Stderr, "transport must be http or grpc")
		os.Exit(2)
	}
	var counter atomic.Int64
	if *warmup > 0 {
		r.step(0, 200, 2*time.Second, *warmup, *maxInFlight, &counter)
	}
	var results []result
	previous := 200
	for _, field := range strings.Split(*rates, ",") {
		rate, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || rate < 1 {
			fmt.Fprintln(os.Stderr, "invalid rate")
			os.Exit(2)
		}
		res := r.step(previous, rate, *ramp, *hold, *maxInFlight, &counter)
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "target %d/s: achieved %.0f/s p50 %.2fms p99 %.2fms dropped %d failures %d incorrect %d\n", rate, res.Achieved, res.LatencyMS.Med, res.LatencyMS.P99, res.Dropped, res.Failures, res.Incorrect)
		previous = rate
		time.Sleep(3 * time.Second) // let connections and the server settle between steps
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(results)
	if *outPath != "" {
		if err := os.WriteFile(*outPath, buffer.Bytes(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Print(buffer.String())
}
