// Command agent-identity-to-network demonstrates correlating Agent Substrate actor
// identity with Cilium/Hubble network flows to answer "which agent talks to
// whom", including external destinations reached through the egress gateway,
// where NAT otherwise erases the actor.
//
// It consumes two streams, shaped exactly like the real signals:
//
//   - egress correlation records: emitted by atenet-egress on the actor
//     telemetry plane. Hold the actor identity, the worker pod/slot, and the
//     post-SNAT 5-tuple of the outbound connection. (This signal is the one
//     piece of new Substrate-side instrumentation the design calls for.)
//
//   - Hubble flows: consumed read-only from Hubble as-is. Hold the observed
//     5-tuple, the policy verdict, and the policy name. The flow's source is
//     the egress gateway pod, not the actor; identity is gone at this layer.
//
// The join key is (5-tuple + time window): the egress record and the Hubble
// flow describe the same connection, so matching them re-attaches the actor
// Hubble could not see.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type EgressRecord struct {
	Time   time.Time `json:"time"`
	Actor  Actor     `json:"actor"`
	Worker Worker    `json:"worker"`
	Conn   Conn      `json:"conn"`
}

type Actor struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Atespace string `json:"atespace"`
}

type Worker struct {
	PodUID  string `json:"pod_uid"`
	PodName string `json:"pod_name"`
	Slot    int    `json:"slot"`
}

type Conn struct {
	SrcIP     string `json:"src_ip"`
	SrcPort   int    `json:"src_port"`
	DstIP     string `json:"dst_ip"`
	DstPort   int    `json:"dst_port"`
	Proto     string `json:"proto"`
	Authority string `json:"authority"`
}

type HubbleFlow struct {
	Time        time.Time   `json:"time"`
	Source      FlowSource  `json:"source"`
	Destination FlowDest    `json:"destination"`
	L4          L4          `json:"l4"`
	Verdict     string      `json:"verdict"`
	Policy      string      `json:"policy"`
}

type FlowSource struct {
	PodName   string `json:"pod_name"`
	Namespace string `json:"namespace"`
	Identity  string `json:"identity"`
}

type FlowDest struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	FQDN string `json:"fqdn"`
}

type L4 struct {
	SrcIP   string `json:"src_ip"`
	SrcPort int    `json:"src_port"`
	DstIP   string `json:"dst_ip"`
	DstPort int    `json:"dst_port"`
	Proto   string `json:"proto"`
}

// fiveTuple is the join key shared by an egress record and the Hubble flow of
// the same connection.
func fiveTuple(proto, srcIP string, srcPort int, dstIP string, dstPort int) string {
	return fmt.Sprintf("%s|%s:%d>%s:%d", proto, srcIP, srcPort, dstIP, dstPort)
}

// Correlated is one answered question: this actor talked to this destination,
// and the network said this about it.
type Correlated struct {
	Rec     EgressRecord
	Flow    *HubbleFlow // nil when no flow observed the connection
}

func main() {
	dataDir := flag.String("data", "data", "directory holding egress_records.json and hubble_flows.json")
	window := flag.Duration("window", 2*time.Second, "max time delta to accept a 5-tuple match")
	flag.Parse()

	var records []EgressRecord
	mustLoad(filepath.Join(*dataDir, "egress_records.json"), &records)
	var flows []HubbleFlow
	mustLoad(filepath.Join(*dataDir, "hubble_flows.json"), &flows)

	// Index flows by 5-tuple. A 5-tuple can recur over time (ports get reused),
	// so keep all and pick the nearest in time at join.
	byTuple := map[string][]HubbleFlow{}
	for _, f := range flows {
		k := fiveTuple(f.L4.Proto, f.L4.SrcIP, f.L4.SrcPort, f.L4.DstIP, f.L4.DstPort)
		byTuple[k] = append(byTuple[k], f)
	}

	var out []Correlated
	for _, r := range records {
		k := fiveTuple(r.Conn.Proto, r.Conn.SrcIP, r.Conn.SrcPort, r.Conn.DstIP, r.Conn.DstPort)
		out = append(out, Correlated{Rec: r, Flow: nearest(byTuple[k], r.Time, *window)})
	}

	printTable(out)
	printGraph(out)
	printAmbiguityContrast(flows)
}

func nearest(cands []HubbleFlow, t time.Time, window time.Duration) *HubbleFlow {
	var best *HubbleFlow
	var bestDelta time.Duration
	for i := range cands {
		d := cands[i].Time.Sub(t)
		if d < 0 {
			d = -d
		}
		if d <= window && (best == nil || d < bestDelta) {
			best = &cands[i]
			bestDelta = d
		}
	}
	return best
}

func printTable(out []Correlated) {
	fmt.Println("WHO TALKS TO WHOM  (actor identity re-attached to NAT'd flows)")
	fmt.Println("================================================================================")
	fmt.Printf("%-13s %-9s %-22s %-8s %-16s %s\n", "AGENT", "ATESPACE", "DESTINATION", "VERDICT", "POLICY", "WORKER")
	fmt.Println("--------------------------------------------------------------------------------")
	for _, c := range out {
		dst := fmt.Sprintf("%s:%d", c.Rec.Conn.Authority, c.Rec.Conn.DstPort)
		verdict, policy := "UNOBSERVED", "-"
		if c.Flow != nil {
			verdict, policy = c.Flow.Verdict, c.Flow.Policy
		}
		worker := fmt.Sprintf("%s#%d", c.Rec.Worker.PodName, c.Rec.Worker.Slot)
		fmt.Printf("%-13s %-9s %-22s %-8s %-16s %s\n",
			c.Rec.Actor.Name, c.Rec.Actor.Atespace, dst, mark(verdict), policy, worker)
	}
	fmt.Println()
}

func mark(verdict string) string {
	switch verdict {
	case "FORWARDED":
		return "allow"
	case "DROPPED":
		return "DENY"
	default:
		return verdict
	}
}

func printGraph(out []Correlated) {
	// Group destinations per agent, stable order.
	type edge struct {
		dst, verdict, policy string
	}
	agents := map[string]*Worker{}
	edges := map[string][]edge{}
	var order []string
	for _, c := range out {
		a := c.Rec.Actor.Atespace + "/" + c.Rec.Actor.Name
		if _, ok := agents[a]; !ok {
			w := c.Rec.Worker
			agents[a] = &w
			order = append(order, a)
		}
		v, p := "UNOBSERVED", "-"
		if c.Flow != nil {
			v, p = c.Flow.Verdict, c.Flow.Policy
		}
		edges[a] = append(edges[a], edge{
			dst:     fmt.Sprintf("%s:%d", c.Rec.Conn.Authority, c.Rec.Conn.DstPort),
			verdict: v, policy: p,
		})
	}
	sort.Strings(order)

	fmt.Println("CORRELATION GRAPH")
	fmt.Println("================================================================================")
	for _, a := range order {
		w := agents[a]
		fmt.Printf("%s  (pod %s#%d)\n", a, w.PodName, w.Slot)
		es := edges[a]
		for i, e := range es {
			branch := "├──►"
			if i == len(es)-1 {
				branch = "└──►"
			}
			sym := "✓"
			if e.verdict != "FORWARDED" {
				sym = "✗"
			}
			fmt.Printf("   %s %-24s %s %-10s policy=%s\n", branch, e.dst, sym, e.verdict, e.policy)
		}
		fmt.Println()
	}
}

// printAmbiguityContrast shows what Hubble alone can say: the flow source is
// the egress gateway, so co-located agents are indistinguishable without the
// egress correlation record.
func printAmbiguityContrast(flows []HubbleFlow) {
	fmt.Println("WHAT HUBBLE SEES ALONE  (no egress record: actor erased by NAT)")
	fmt.Println("================================================================================")
	for _, f := range flows {
		fmt.Printf("   %s ──► %s:%d   %-9s  (which agent? unknown, source is the gateway)\n",
			f.Source.Identity, f.Destination.FQDN, f.Destination.Port, mark(f.Verdict))
	}
	fmt.Println()
}

func mustLoad(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := json.Unmarshal(b, v); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", path, err)
		os.Exit(1)
	}
}
