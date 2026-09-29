// Package ports は、projectの帯と枠ごとのportの割り当てを決める。
package ports

const (
	// BandSize は、1つのprojectに割り当てるportの帯の大きさ。
	BandSize = 1000
	// PerSlot は、1つの枠が使えるportの数。枠の先頭は PerSlot ずつ離れる。
	PerSlot = 100
	// MaxSlots は、1つのprojectが持てる枠の最大数。
	MaxSlots = BandSize / PerSlot
	// Max は、割り当てるportの上限。
	Max = 65535
)

// Port は、名前の付いたportである。
type Port struct {
	Name   string
	Number int
}

// Assign は、枠のportをnamesの順に返す。
// portは、帯の先頭＋PerSlot×(slot−1)＋namesの中の順番。
func Assign(names []string, base, slot int) []Port {
	out := make([]Port, len(names))
	for i, n := range names {
		out[i] = Port{Name: n, Number: base + PerSlot*(slot-1) + i}
	}
	return out
}

// Map は、portを名前から番号へのmapにする。
func Map(ps []Port) map[string]int {
	m := make(map[string]int, len(ps))
	for _, p := range ps {
		m[p.Name] = p.Number
	}
	return m
}
