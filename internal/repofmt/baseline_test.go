package repofmt

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHeadlineClaimsMatchTheBaseline pins the numbers README.md leads with
// against the measured records they come from.
//
// WHY IT EXISTS. The first screen of the README invites the reader to check
// seven quantified claims, and docs/baseline/ is the check: those files are the
// record of runs that already happened, exempt from every other prose rewrite
// here for exactly that reason. Nothing tied the two together, so a number
// re-typed from 58.97 to 58.9, or a summary regenerated with a different verdict,
// would leave the README asserting what the record no longer says -- and the
// record is the only reason to believe the sentence.
//
// WHAT IT CHECKS. Each entry reads one number out of a named baseline file and
// requires the string a document prints to be that number formatted the way that
// document formats it (the scale and the number of decimals are part of the entry,
// because "58.97%" is stored as 0.5897 and "2.42x" as 2.423). An entry names the
// documents that have to print it -- README.md when it names none -- because the
// same numbers carry the first screen of the README and the one-page RESUME, and
// the two are edited at different times by whoever is in a hurry. A path may start
// with "*", which searches the whole file for that key: it succeeds only when the
// key carries the same value everywhere it appears, so a second copy with a
// different number is a failure rather than a coin flip. When a key is not unique
// by name alone (m4-summary.json has one ratio per variant, m2-summary.json a
// hit_rate per sweep step) the entry names the object it means with a marker, so
// adding a variant or a step cannot silently move which number is read. An entry
// may also name several files and ask for their sum, which is how a total spread
// over the three load runs is read.
//
// WHAT IT DOES NOT CHECK. Whether the run was any good, or whether the baseline
// still describes today's code: a baseline is one afternoon's measurement on one
// machine, and the prose says so. This only settles that the sentence and the
// record are about the same number. Nor does it follow a number nobody quotes: a
// reading that only lives in the JSON is the JSON's business.
func TestHeadlineClaimsMatchTheBaseline(t *testing.T) {
	root := repoRoot(t)
	readme := readFile(t, filepath.Join(root, "README.md"))

	// The numbers below lead the README and the RESUME alike; the ones the RESUME
	// does not carry are the README's alone, and the measurements of the local
	// inference story are the RESUME's alone.
	readmeAndResume := []claimPrint{{}, {file: "docs/RESUME.md"}}
	resumeOnly := []claimPrint{{file: "docs/RESUME.md"}}

	claims := []baselineClaim{
		{
			quote: "420.42", file: "m1-summary.json", docs: readmeAndResume,
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_while_breaker_closed"}, digits: 2,
		},
		{
			quote: "17.81", file: "m1-summary.json", docs: readmeAndResume,
			find: map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"},
			path: []string{"median_ms_once_breaker_open"}, digits: 2,
		},
		{
			quote: "58.97", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"hit_rate", "hit_rate"}, scale: 100, digits: 2,
		},
		{
			quote: "56.39", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"*", "reduction_percent"}, digits: 2,
		},
		{
			// The cached path and the uncached one, as the M2 row of the README
			// table and the cache paragraph of the RESUME both quote them.
			quote: "5.50", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"latency", "gateway_stats", "cache_on_pure_hit", "stats", "p95_ms"}, digits: 2,
		},
		{
			quote: "47.31", file: "m2-summary.json", docs: readmeAndResume,
			path: []string{"latency", "gateway_stats", "cache_off_pure_miss", "stats", "p95_ms"}, digits: 2,
		},
		{
			quote: "100.7", file: "m4-summary.json", docs: readmeAndResume,
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "variant"}, digits: 1,
		},
		{
			quote: "2.42", file: "m4-summary.json", docs: readmeAndResume,
			find: map[string]any{"variant": "awq"},
			path: []string{"output_tokens_per_s", "ratio"}, digits: 2,
		},
		{
			quote: "34.52", file: "m4-summary.json", docs: readmeAndResume,
			path: []string{"*", "avoided_percent"}, digits: 2,
		},
		{
			// The RESUME tells the local-inference story as an end-to-end P50 that
			// fell from the fp16 run to the quantized one; the README does not.
			quote: "1758", phrase: "1758ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"total_ms", "p50"}, digits: 0,
		},
		{
			quote: "700", phrase: "700ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"total_ms", "p50"}, digits: 0,
		},
		{
			// The RESUME prints the M4 table at one decimal per cell, while the
			// summary sentence above rounds the same readings to whole
			// milliseconds. Both are quoted as printed: the table is the artifact
			// a reader compares variants with, so its cells get their own entries.
			quote: "35.2", phrase: "35.2ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"ttft_ms", "p50"}, digits: 1,
		},
		{
			quote: "28.1", phrase: "28.1ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"ttft_ms", "p50"}, digits: 1,
		},
		{
			quote: "28.7", phrase: "28.7ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"ttft_ms", "p50"}, digits: 1,
		},
		{
			quote: "1758.5", phrase: "1758.5ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"total_ms", "p50"}, digits: 1,
		},
		{
			quote: "699.5", phrase: "699.5ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"total_ms", "p50"}, digits: 1,
		},
		{
			quote: "723.6", phrase: "723.6ms", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"total_ms", "p50"}, digits: 1,
		},
		{
			quote: "41.5", phrase: "41.5 tok/s", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"throughput", "output_tokens_per_s_request_wall"}, digits: 1,
		},
		{
			quote: "95.3", phrase: "95.3 tok/s", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"throughput", "output_tokens_per_s_request_wall"}, digits: 1,
		},
		{
			// Token overlap is a set-Jaccard mean against the fp16 texts, not an
			// accuracy score; the record says so in a caveat beside the number.
			quote: "0.444", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"agreement", "mean_token_overlap"}, digits: 3,
		},
		{
			quote: "0.460", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"agreement", "mean_token_overlap"}, digits: 3,
		},
		{
			// The exact-match column is a rate over 12 requests; scaling it back up
			// to a count is what makes the RESUME's "0/12" and "1/12" checkable.
			quote: "1", phrase: "1/12", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"agreement", "exact_match_rate"}, digits: 0, scale: 12,
		},
		{
			quote: "0", phrase: "0/12", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"agreement", "exact_match_rate"}, digits: 0, scale: 12,
		},
		{
			// VRAM is quoted as the delta a load reported, in the MiB the RESUME
			// prints even though the record names its field _mb.
			quote: "6169", phrase: "+6169 MiB", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"vram_delta_mb"}, digits: 0,
		},
		{
			quote: "6921", phrase: "+6921 MiB", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"vram_delta_mb"}, digits: 0,
		},
		{
			quote: "7353", phrase: "+7353 MiB", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"vram_delta_mb"}, digits: 0,
		},
		{
			quote: "58", phrase: "58s", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "fp16"},
			path: []string{"load_seconds"}, digits: 0,
		},
		{
			quote: "60", phrase: "60s", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "awq"},
			path: []string{"load_seconds"}, digits: 0,
		},
		{
			quote: "55", phrase: "55s", file: "m4-summary.json", docs: resumeOnly,
			find: map[string]any{"variant": "gptq"},
			path: []string{"load_seconds"}, digits: 0,
		},
		{
			quote: "59400", file: "m5-summary.json", docs: readmeAndResume,
			path: []string{"*", "otlp_exported"}, digits: 0,
		},
		{
			quote: "22.07", file: "m5-summary.json", docs: readmeAndResume,
			find: map[string]any{"workload": "non-stream", "concurrency": 128.0},
			path: []string{"direct_minus_gateway_pct"}, digits: 2,
		},
		{
			// The idempotency payoff of the RESUME is B_replay_payoff in the M6
			// record: fresh generation against the replayed answer over 20 pairs.
			// The fresh column is paced by the mock, which the RESUME says out
			// loud next to the ratio, so the ratio is quoted rather than sold.
			quote: "5.7", phrase: "5.7ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "fresh_median_ms"}, digits: 1,
		},
		{
			quote: "8.1", phrase: "8.1ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "fresh_p95_ms"}, digits: 1,
		},
		{
			quote: "4.5", phrase: "4.5ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "replay_median_ms"}, digits: 1,
		},
		{
			quote: "24.9", phrase: "24.9ms", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "replay_p95_ms"}, digits: 1,
		},
		{
			quote: "1.267", file: "m6-summary.json", docs: resumeOnly,
			path: []string{"*", "latency_ratio_fresh_over_replay"}, digits: 3,
		},
		{
			// "重放期间 provider 调用 0 次" is not a field of its own: the B
			// block keeps one row per pair and every one of them saw the mock
			// not move, so the prose's zero is that column's sum. The README
			// prints it in two shapes and the RESUME counts the pairs, so all
			// three wordings hang off the same sum.
			quote: "0", sum: true, file: "m6-summary.json",
			find: map[string]any{"replay_upstream": "replay"},
			path: []string{"mock_calls_replay"}, digits: 0,
			docs: []claimPrint{
				{phrase: "重放期间 provider 调用 **0** 次"},
				{phrase: "同 key 重放 0 次 provider 调用"},
				{file: "docs/RESUME.md", phrase: "20/20 次重放没有打到上游"},
			},
		},
		{
			// A sum of zero over no rows at all would read the same, so the
			// pair count is pinned as well: the claim above needs rows to sum.
			quote: "20", file: "m6-summary.json",
			find: map[string]any{"question": "what does a replayed turn buy?"},
			path: []string{"samples"}, digits: 0,
			docs: []claimPrint{
				{file: "docs/RESUME.md", phrase: "20 对"},
				{file: "docs/USAGE.md", phrase: "重放 20 对"},
				{file: "docs/DESIGN.md", phrase: "打 20 对"},
			},
		},
		{
			// The storage arm of the M6 record: a cap of 256 keys, 300 distinct
			// keys pumped through it one at a time, and what the admin endpoint
			// and the metric reported afterwards. The RESUME quotes them in one
			// paragraph and says what the byte figures do not claim.
			quote: "300", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"keys_pumped"}, digits: 0,
		},
		{
			quote: "256", phrase: "容量 256", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"capacity"}, digits: 0,
		},
		{
			quote: "256", phrase: "封顶 256/256", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"stored_final"}, digits: 0,
		},
		{
			quote: "320", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"stats_stored"}, digits: 0,
		},
		{
			quote: "340", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"stats_lookups"}, digits: 0,
		},
		{
			quote: "44", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"stats_evicted"}, digits: 0,
		},
		{
			quote: "44", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"metric_evicted_total"}, digits: 0,
		},
		{
			// The provider was asked once per distinct key, which is what the
			// RESUME means by "asked exactly 300 times".
			quote: "300", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"provider_calls"}, digits: 0,
		},
		{
			quote: "285", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"answer_bytes"}, digits: 0,
		},
		{
			// 72960 bytes over 256 entries is a floor, which is why the record
			// keeps bytes_per_stored_entry null and the RESUME declines to claim
			// a number per entry. The claim reads the bytes, because the prose's
			// 71.3 KiB rounds that exact 71.25 up while Go rounds it down.
			quote: "72960", phrase: "71.3 KiB", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"stored_answer_bytes_floor"}, digits: 0,
		},
		{
			quote: "174.2", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"sequential_qps"}, digits: 1,
		},
		{
			quote: "5.3", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"sequential_median_ms"}, digits: 1,
		},
		{
			// RSS is quoted in MiB and with a real minus sign (U+2212) in the
			// prose, so that phrase is spelled out instead of left to the
			// number, which formats with an ASCII hyphen.
			quote: "-4.05", phrase: "−4.05 MiB", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"keys_pumped": 300.0},
			path: []string{"rss_growth_bytes"}, digits: 2, scale: 1.0 / 1048576,
		},
		{
			// The concurrency arm: sixteen clients through one barrier against
			// one key, one provider call, no second generation.
			quote: "16", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"clients"}, digits: 0,
		},
		{
			quote: "1", phrase: "1 × 200 新答案", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"observed_mix", "produced_200_fresh"}, digits: 0,
		},
		{
			quote: "15", phrase: "15 × 409 in-flight", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"observed_mix", "in_flight_409"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 × 200 重放", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"observed_mix", "replayed_200"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 其他", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"observed_mix", "other"}, digits: 0,
		},
		{
			quote: "1", phrase: "恰好 **+1**", file: "m6-summary.json", docs: resumeOnly,
			find: map[string]any{"window": "300ms"},
			path: []string{"provider_calls"}, digits: 0,
		},
		{
			quote: "13", phrase: "13 对近似语料", file: "m2-corpus.json",
			path: []string{"should_hit"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 误命中", file: "m2-summary.json",
			find: map[string]any{"should_not_hit_requests": 26.0},
			path: []string{"false_hits"}, digits: 0,
		},
		{
			quote: "16", phrase: "16 个请求", file: "m4-summary.json",
			path: []string{"tiering", "mix", "requests_total"}, digits: 0,
		},
		{
			quote: "20", phrase: "20/20", file: "m3-summary.json",
			path: []string{"fail_closed", "requests_during_outage"}, digits: 0,
		},
		{
			quote: "20", phrase: "20/20", file: "m3-summary.json",
			path: []string{"fail_closed", "outage_status_codes", "503"}, digits: 0,
		},
		{
			quote: "0", phrase: "上游 0 调用", file: "m3-summary.json",
			path: []string{"fail_closed", "upstream_calls_during_outage"}, digits: 0,
		},
		{
			// The M2 cost readings are an extrapolation from a configured price
			// table rather than a bill: the measured workload cost divided by its
			// request count, scaled to 1000. The record says so in its own
			// "extrapolation" note.
			quote: "0.036692", file: "m2-summary.json",
			path: []string{"cost", "cost_per_1k_requests_without_cache"}, digits: 6,
		},
		{
			quote: "0.016", file: "m2-summary.json",
			path: []string{"cost", "cost_per_1k_requests_with_cache"}, digits: 3,
		},
		{
			// The M3 reservation ledger. Each document phrases it its own way,
			// and both phrases are checked.
			quote: "2872", file: "m3-summary.json",
			docs: []claimPrint{
				{phrase: "预扣 2872"},
				{file: "docs/RESUME.md", phrase: "预扣 **2872**"},
			},
			path: []string{"accuracy", "reserved_tokens_delta"}, digits: 0,
		},
		{
			quote: "320", file: "m3-summary.json",
			docs: []claimPrint{
				{phrase: "结算 320"},
				{file: "docs/RESUME.md", phrase: "真实结算 **320**"},
			},
			path: []string{"accuracy", "settled_tokens_delta"}, digits: 0,
		},
		{
			quote: "2608", file: "m3-summary.json",
			docs: []claimPrint{
				{phrase: "释放 2608"},
				{file: "docs/RESUME.md", phrase: "返还 **2608**"},
			},
			path: []string{"accuracy", "released_tokens_delta"}, digits: 0,
		},
		{
			quote: "56", file: "m3-summary.json",
			docs: []claimPrint{
				{phrase: "超发 56"},
				{file: "docs/RESUME.md", phrase: "超支 **56** token"},
			},
			path: []string{"accuracy", "overshoot_tokens"}, digits: 0,
		},
		{
			quote: "171", phrase: "**171** 微美元", file: "m3-summary.json", docs: resumeOnly,
			path: []string{"accuracy", "overshoot_cost_micros"}, digits: 0,
		},
		{
			quote: "222", phrase: "平均绝对偏差 222", file: "m3-summary.json", docs: resumeOnly,
			path: []string{"accuracy", "mean_absolute_deviation_tokens"}, digits: 0,
		},
		{
			// The record holds 212.667; the prose prints it to one decimal and
			// with the plus sign it uses for a signed deviation.
			quote: "212.7", phrase: "平均有符号偏差 +212.7", file: "m3-summary.json", docs: resumeOnly,
			path: []string{"accuracy", "mean_signed_deviation_tokens"}, digits: 1,
		},
		{
			// The M3 budget arm: forty requests against a 280-token day budget,
			// sent one at a time so each reservation settles before the next
			// admission.
			quote: "40", phrase: "40/40", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"without_quota", "client_status_codes", "200"}, digits: 0,
		},
		{
			quote: "1", phrase: "1×200", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"with_budget", "client_status_codes", "200"}, digits: 0,
		},
		{
			quote: "39", phrase: "39×429", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"with_budget", "client_status_codes", "429"}, digits: 0,
		},
		{
			quote: "97.5", phrase: "97.5% 被拒", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"with_budget", "client_429_rate_percent"}, digits: 1,
		},
		{
			quote: "9", phrase: "停在 9", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"with_budget", "tenant_tokens_today_after"}, digits: 0,
		},
		{
			// Both ends of the "upstream calls 40 -> 1" arrow, each read from
			// its own arm.
			quote: "40", phrase: "上游调用 **40 → 1**", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"without_quota", "upstream_calls"}, digits: 0,
		},
		{
			quote: "1", phrase: "上游调用 **40 → 1**", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"with_budget", "upstream_calls"}, digits: 0,
		},
		{
			// The no-budget arm's wall clock. It is a timing, so a regeneration
			// moves it; the gate failing there is the point, since the prose
			// states it.
			quote: "4.11", phrase: "（40×200，4.11s）", file: "m3-summary.json", docs: resumeOnly,
			find: map[string]any{"budget_tokens_per_day": 280.0},
			path: []string{"without_quota", "wall_s"}, digits: 2,
		},
		{
			// The two documents word this one differently: the README counts
			// requests, the RESUME just says how many there were.
			quote: "18000", file: "m1-summary.json", sum: true,
			docs: []claimPrint{
				{phrase: "18000 个请求"},
				{file: "docs/RESUME.md", phrase: "18000 请求"},
			},
			files: []string{"m1-load-faulted-r1.json", "m1-load-faulted-r2.json", "m1-load-faulted-r3.json"},
			path:  []string{"*", "requests"}, digits: 0,
		},
		{
			quote: "0", phrase: "0 错误", sum: true,
			files: []string{"m1-load-faulted-r1.json", "m1-load-faulted-r2.json", "m1-load-faulted-r3.json"},
			path:  []string{"*", "errors"}, digits: 0,
		},
	}
	claims = append(claims, m0Claims()...)
	claims = append(claims, m4SecondCopies()...)
	if len(claims) < 147 {
		t.Fatalf("only %d claims; this check has been hollowed out", len(claims))
	}

	parsed := map[string]any{}
	docFor := func(name string) any {
		doc, ok := parsed[name]
		if !ok {
			doc = parseBaseline(t, root, name)
			parsed[name] = doc
		}
		return doc
	}

	texts := map[string]string{"README.md": readme}
	textOf := func(name string) string {
		text, ok := texts[name]
		if !ok {
			text = readFile(t, filepath.Join(root, name))
			texts[name] = text
		}
		return text
	}

	for _, claim := range claims {
		names := claim.files
		if len(names) == 0 {
			names = []string{claim.file}
		}

		var values []float64
		for _, name := range names {
			found := numbersAt(docFor(name), claim.path)
			if len(claim.find) > 0 {
				found = nil
				for _, object := range findObjects(docFor(name), claim.find) {
					found = append(found, numbersAt(object, claim.path)...)
				}
			}
			if len(found) == 0 {
				t.Errorf("docs/baseline/%s has no value at %s%s; the record moved",
					name, marker(claim.find), strings.Join(claim.path, "."))
			}
			values = append(values, found...)
		}
		if len(values) == 0 {
			continue
		}

		total := values[0]
		if claim.sum {
			total = 0
			for _, value := range values {
				total += value
			}
		} else {
			distinct := map[float64]bool{}
			for _, value := range values {
				distinct[value] = true
			}
			if len(distinct) > 1 {
				t.Errorf("docs/baseline/%s carries %d different values for %s%s; name the one this claim means",
					strings.Join(names, ", "), len(distinct), marker(claim.find), strings.Join(claim.path, "."))
				continue
			}
		}

		factor := claim.scale
		if factor == 0 {
			factor = 1
		}
		got := strconv.FormatFloat(total*factor, 'f', claim.digits, 64)
		if got != claim.quote {
			t.Errorf("docs/baseline/%s%s reads %s, but %s prints %s",
				strings.Join(names, ", "), marker(claim.find), got,
				strings.Join(claimDocs(claim), " and "), claim.quote)
		}
		for _, want := range claim.prints() {
			doc, phrase := want.document(), want.wording(claim)
			if !strings.Contains(textOf(doc), phrase) {
				t.Errorf("%s no longer prints %s anywhere; if the claim moved, move this entry too", doc, phrase)
			}
		}
	}

	// The 23.6x in the M1 bullet is arithmetic on the two readings above rather
	// than a field of its own, so it is computed here from the same record.
	closed := singleNumber(t, docFor("m1-summary.json"),
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, []string{"median_ms_while_breaker_closed"})
	open := singleNumber(t, docFor("m1-summary.json"),
		map[string]any{"scenario": "stalled-primary behind a per-attempt timeout"}, []string{"median_ms_once_breaker_open"})
	factor := strconv.FormatFloat(closed/open, 'f', 1, 64)
	if factor != "23.6" {
		t.Errorf("m1-summary.json's two medians give %s, but the prose prints 23.6", factor)
	}
	for _, doc := range readmeAndResume {
		if !strings.Contains(textOf(doc.document()), factor+"×") {
			t.Errorf("%s does not print %s next to the two medians", doc.document(), factor+"×")
		}
	}

	// "16 requests split 8:8" is a total plus how it divides, so the split is
	// checked as a shape rather than as one more number.
	mix := docFor("m4-summary.json")
	simple := singleNumber(t, mix, nil, []string{"tiering", "mix", "simple_per_kind"})
	hard := singleNumber(t, mix, nil, []string{"tiering", "mix", "hard_per_kind"})
	split := strconv.FormatFloat(simple, 'f', 0, 64) + ":" + strconv.FormatFloat(hard, 'f', 0, 64)
	if simple != hard {
		t.Errorf("m4-summary.json splits %v simple and %v hard requests, so it is not the even split README.md prints",
			simple, hard)
	} else if !strings.Contains(readme, split) {
		t.Errorf("m4-summary.json's split is %s, but README.md does not print it", split)
	}

	// "省 11 token/轮" is the two per-turn usage counts added up rather than a
	// field of its own, so it is computed here from the same record.
	turns := docFor("m6-summary.json")
	perTurn := map[string]any{"prompt_tokens": 6.0}
	avoided := singleNumber(t, turns, perTurn, []string{"prompt_tokens"}) +
		singleNumber(t, turns, perTurn, []string{"completion_tokens"})
	if avoided != 11 {
		t.Errorf("m6-summary.json's per-turn usage adds up to %v, but the prose prints 11 token", avoided)
	}
	for _, want := range []claimPrint{
		{phrase: "省 11 token/轮"},
		{file: "docs/RESUME.md", phrase: "11 token"},
	} {
		if !strings.Contains(textOf(want.document()), want.phrase) {
			t.Errorf("%s no longer prints %s; the replay payoff moved", want.document(), want.phrase)
		}
	}

	t.Logf("checked %d headline claims against docs/baseline", len(claims)+2)
}

// baselineClaim is one number the prose prints, and where its record keeps it.
type baselineClaim struct {
	quote  string         // exactly what the document prints, already scaled and rounded
	file   string         // file under docs/baseline/
	files  []string       // several records, when the claim is their total
	sum    bool           // add the values instead of requiring them to agree
	phrase string         // wording that carries the number; the quote itself when empty
	docs   []claimPrint   // documents that must print it; README.md when it names none
	find   map[string]any // key/value pairs naming the object to read, when the path alone is ambiguous
	path   []string       // keys from that object down to the number; a leading "*" searches the file
	scale  float64        // 100 to turn a stored ratio into the percent the prose prints; 0 means none
	digits int            // decimals the document prints
}

// claimPrint is one document that has to carry a claim, and the wording it uses:
// the same measurement is "18000 个请求" in the README and "18000 请求" in the
// RESUME, and what matters is that both still say the number at all.
type claimPrint struct {
	file   string // document; README.md when empty
	phrase string // the claim's phrase, then the quote itself, when empty
}

// prints is where a claim has to appear: the documents it names, or the README,
// which is what an entry means when it names none.
func (claim baselineClaim) prints() []claimPrint {
	if len(claim.docs) > 0 {
		return claim.docs
	}
	return []claimPrint{{}}
}

func (want claimPrint) document() string {
	if want.file != "" {
		return want.file
	}
	return "README.md"
}

func (want claimPrint) wording(claim baselineClaim) string {
	if want.phrase != "" {
		return want.phrase
	}
	if claim.phrase != "" {
		return claim.phrase
	}
	return claim.quote
}

// claimDocs names the documents a claim has to appear in, for error messages.
func claimDocs(claim baselineClaim) []string {
	prints := claim.prints()
	names := make([]string, 0, len(prints))
	for _, want := range prints {
		names = append(names, want.document())
	}
	return names
}

func marker(find map[string]any) string {
	if len(find) == 0 {
		return ""
	}
	parts := make([]string, 0, len(find))
	for key, value := range find {
		parts = append(parts, key+"="+strconv.Quote(strings.TrimSpace(toString(value))))
	}
	return " where " + strings.Join(parts, " ") + ":"
}

// toString renders a marker value for an error message. Numbers and booleans
// are worth rendering: the M0 rows are selected by concurrency, and a message
// that says concurrency="" tells the reader nothing about which row matched.
func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

func parseBaseline(t *testing.T, root, name string) any {
	t.Helper()

	var doc any
	raw := readFile(t, filepath.Join(root, "docs", "baseline", name))
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("docs/baseline/%s is not JSON: %v", name, err)
	}
	return doc
}

// findObjects returns every object in the document that carries all the marker's
// key/value pairs, so a claim can name its block instead of counting array
// positions that a new variant would shift.
func findObjects(node any, find map[string]any) []map[string]any {
	var found []map[string]any
	var walk func(any)
	walk = func(n any) {
		switch typed := n.(type) {
		case map[string]any:
			match := true
			for key, want := range find {
				if got, ok := typed[key]; !ok || got != want {
					match = false
					break
				}
			}
			if match {
				found = append(found, typed)
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(node)
	return found
}

// numbersAt walks a path to a number, collecting every value it finds: a leading
// "*" searches the whole document for the next key name, and an integer segment
// indexes an array.
func numbersAt(node any, path []string) []float64 {
	if len(path) == 0 {
		if value, ok := node.(float64); ok {
			return []float64{value}
		}
		return nil
	}
	if path[0] == "*" {
		if len(path) < 2 {
			return nil
		}
		var out []float64
		var walk func(any)
		walk = func(n any) {
			switch typed := n.(type) {
			case map[string]any:
				for key, child := range typed {
					if key == path[1] {
						out = append(out, numbersAt(child, path[2:])...)
					}
					walk(child)
				}
			case []any:
				for _, child := range typed {
					walk(child)
				}
			}
		}
		walk(node)
		return out
	}
	switch typed := node.(type) {
	case map[string]any:
		if child, ok := typed[path[0]]; ok {
			return numbersAt(child, path[1:])
		}
	case []any:
		if index, err := strconv.Atoi(path[0]); err == nil && index >= 0 && index < len(typed) {
			return numbersAt(typed[index], path[1:])
		}
	}
	return nil
}

func singleNumber(t *testing.T, doc any, find map[string]any, path []string) float64 {
	t.Helper()

	var values []float64
	if len(find) == 0 {
		values = numbersAt(doc, path)
	} else {
		for _, object := range findObjects(doc, find) {
			values = append(values, numbersAt(object, path)...)
		}
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly one value at %s, found %d", strings.Join(path, "."), len(values))
	}
	return values[0]
}

// m0Claims pins the M0 comparison table in the RESUME: one row per phase in
// docs/baseline/m0-baseline.json, every cell of a row read from that phase, and
// the row's wording checked as one string, so a row that changes shape fails
// even when the cell that moved is not the cell this entry reads. The phases
// the README quotes by name are checked there as well as in the RESUME.
func m0Claims() []baselineClaim {
	type cell struct {
		claim  baselineClaim
		readme string // the README's wording, when it carries this one too
	}
	rows := []struct {
		label       string
		concurrency float64
		stream      bool
		phrase      string
		usage       string // docs/USAGE.md prints this phase too, with fewer columns
		cells       []cell
	}{
		{
			label: "direct non-stream", concurrency: 8, stream: false,
			phrase: "8393 [6678..10531] | 510us | 2.00ms | 3.00ms",
			usage:  "| 直连上游（非流式） | 8 | 8393 [6678..10531] | 2.00ms | - |",
			cells: []cell{
				{claim: baselineClaim{quote: "8393", path: []string{"qps"}, digits: 0}, readme: "直连 c=8 8393 QPS"},
				{claim: baselineClaim{quote: "6678", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "10531", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "510", path: []string{"p50"}, digits: 0, scale: 1e-3}},
				{claim: baselineClaim{quote: "2.00", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "3.00", path: []string{"p99"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "direct non-stream", concurrency: 32, stream: false,
			phrase: "7831 [5666..12109] | 3.50ms | 8.02ms | 12.01ms",
			cells: []cell{
				{claim: baselineClaim{quote: "7831", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "5666", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "12109", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "3.50", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "8.02", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "12.01", path: []string{"p99"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "gateway non-stream", concurrency: 8, stream: false,
			phrase: "5026 [2837..7359] | 1.00ms | 3.50ms | 4.50ms",
			usage:  "| 经网关（非流式） | 8 | 5026 [2837..7359] | 3.50ms | - |",
			cells: []cell{
				{claim: baselineClaim{quote: "5026", path: []string{"qps"}, digits: 0}, readme: "经网关 5026 QPS"},
				{claim: baselineClaim{quote: "2837", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "7359", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "1.00", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "3.50", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "4.50", path: []string{"p99"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "gateway non-stream", concurrency: 32, stream: false,
			phrase: "5496 [5140..7477] | 5.02ms | 11.00ms | 15.76ms",
			usage:  "| 经网关（非流式） | 32 | 5496 [5140..7477] | 11.00ms | - |",
			cells: []cell{
				{claim: baselineClaim{quote: "5496", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "5140", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "7477", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "5.02", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "11.00", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "15.76", path: []string{"p99"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "direct stream", concurrency: 8, stream: true,
			phrase: "1728 [1655..2297] | 4.50ms | 7.00ms | 8.03ms | 502us | 2.00ms",
			usage:  "| 直连上游（流式） | 8 | 1728 [1655..2297] | 7.00ms | 2.00ms |",
			cells: []cell{
				{claim: baselineClaim{quote: "1728", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "1655", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "2297", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "4.50", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "7.00", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "8.03", path: []string{"p99"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "502", path: []string{"ttft_p50"}, digits: 0, scale: 1e-3}},
				{claim: baselineClaim{quote: "2.00", path: []string{"ttft_p95"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "direct stream", concurrency: 32, stream: true,
			phrase: "1771 [1311..1771] | 20.02ms | 27.45ms | 34.91ms | 1.00ms | 3.50ms",
			cells: []cell{
				{claim: baselineClaim{quote: "1771", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "1311", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "1771", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "20.02", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "27.45", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "34.91", path: []string{"p99"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "1.00", path: []string{"ttft_p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "3.50", path: []string{"ttft_p95"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "gateway stream", concurrency: 8, stream: true,
			phrase: "1088 [820..1144] | 7.00ms | 11.09ms | 13.25ms | 999us | 2.74ms",
			usage:  "| 经网关（流式） | 8 | 1088 [820..1144] | 11.09ms | 2.74ms |",
			cells: []cell{
				{claim: baselineClaim{quote: "1088", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "820", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "1144", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "7.00", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "11.09", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "13.25", path: []string{"p99"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "999", path: []string{"ttft_p50"}, digits: 0, scale: 1e-3}},
				{claim: baselineClaim{quote: "2.74", path: []string{"ttft_p95"}, digits: 2, scale: 1e-6}},
			},
		},
		{
			label: "gateway stream", concurrency: 32, stream: true,
			phrase: "1324 [1095..2125] | 22.86ms | 41.55ms | 51.55ms | 2.00ms | 10.81ms",
			usage:  "| 经网关（流式） | 32 | 1324 [1095..2125] | 41.55ms | 10.81ms |",
			cells: []cell{
				{claim: baselineClaim{quote: "1324", path: []string{"qps"}, digits: 0}},
				{claim: baselineClaim{quote: "1095", path: []string{"qps_min"}, digits: 0}},
				{claim: baselineClaim{quote: "2125", path: []string{"qps_max"}, digits: 0}},
				{claim: baselineClaim{quote: "22.86", path: []string{"p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "41.55", path: []string{"p95"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "51.55", path: []string{"p99"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "2.00", path: []string{"ttft_p50"}, digits: 2, scale: 1e-6}},
				{claim: baselineClaim{quote: "10.81", path: []string{"ttft_p95"}, digits: 2, scale: 1e-6}},
			},
		},
	}

	resumeOnly := []claimPrint{{file: "docs/RESUME.md"}}
	var claims []baselineClaim
	for _, row := range rows {
		for _, c := range row.cells {
			claim := c.claim
			claim.file = "m0-baseline.json"
			claim.find = map[string]any{"label": row.label, "concurrency": row.concurrency, "stream": row.stream}
			claim.phrase = row.phrase
			if c.readme != "" {
				claim.docs = []claimPrint{{file: "README.md", phrase: c.readme}, {file: "docs/RESUME.md"}}
			} else {
				claim.docs = resumeOnly
			}
			claims = append(claims, claim)
		}
		// docs/USAGE.md carries six of these phases in a narrower table, so each
		// of those rows needs its own wording checked; the cell it shows is the
		// median.
		if row.usage != "" {
			for _, c := range row.cells {
				if len(c.claim.path) == 1 && c.claim.path[0] == "qps" {
					claim := c.claim
					claim.file = "m0-baseline.json"
					claim.find = map[string]any{"label": row.label, "concurrency": row.concurrency, "stream": row.stream}
					claim.docs = []claimPrint{{file: "docs/USAGE.md", phrase: row.usage}}
					claims = append(claims, claim)
				}
			}
		}
	}
	return claims
}

// m4SecondCopies pins the two other copies of the M4 quantization readings. The
// RESUME's copy is checked cell by cell above; docs/USAGE.md prints the same
// three rows with fewer columns and docs/DESIGN.md prints the same readings as
// prose, and a number that moves in one of those two places is exactly the drift
// nothing was watching. Each entry names the whole USAGE row, or the DESIGN
// sentence it sits in, as its wording, so a wrong column or a rewritten sentence
// fails even when the field that entry reads is a different one.
func m4SecondCopies() []baselineClaim {
	type copyClaim struct {
		variant string
		path    []string
		quote   string
		digits  int
		doc     string
		phrase  string
	}
	copies := []copyClaim{
		{
			variant: "fp16", path: []string{"ttft_ms", "p50"}, quote: "35.2", digits: 1,
			doc:    "docs/USAGE.md",
			phrase: "| FP16（参照） | 3.09 GB | 58s | 35.2ms | 1758.5ms | 41.5 | 6169 MiB | — | — |",
		},
		{
			variant: "awq", path: []string{"ttft_ms", "p50"}, quote: "28.1", digits: 1,
			doc:    "docs/USAGE.md",
			phrase: "| AWQ（4bit, group 128） | 1.61 GB | 60s | 28.1ms | 699.5ms | **100.7** | 6921 MiB | 0/12 | 0.444 |",
		},
		{
			variant: "gptq", path: []string{"ttft_ms", "p50"}, quote: "28.7", digits: 1,
			doc:    "docs/USAGE.md",
			phrase: "| GPTQ-Int4 | 1.15 GB | 55s | 28.7ms | 723.6ms | 95.3 | 7353 MiB | 1/12 | 0.460 |",
		},
		{
			variant: "fp16", path: []string{"total_ms", "p50"}, quote: "1758.5", digits: 1,
			doc:    "docs/DESIGN.md",
			phrase: "fp16 首字 P50 35.2ms / 端到端 P50 1758.5ms / 41.5 tok·s⁻¹ / 权重 3.09 GB",
		},
		{
			variant: "awq", path: []string{"throughput", "output_tokens_per_s_request_wall"}, quote: "100.7", digits: 1,
			doc:    "docs/DESIGN.md",
			phrase: "100.7 tok·s⁻¹ / 1.61 GB",
		},
		{
			variant: "gptq", path: []string{"total_ms", "p50"}, quote: "723.6", digits: 1,
			doc:    "docs/DESIGN.md",
			phrase: "GPTQ-Int4 28.7ms / 723.6ms / 95.3 tok·s⁻¹ / 1.15 GB",
		},
	}

	claims := make([]baselineClaim, 0, len(copies))
	for _, c := range copies {
		claims = append(claims, baselineClaim{
			quote: c.quote, file: "m4-summary.json", find: map[string]any{"variant": c.variant},
			path: c.path, digits: c.digits,
			docs: []claimPrint{{file: c.doc, phrase: c.phrase}},
		})
	}
	return claims
}
