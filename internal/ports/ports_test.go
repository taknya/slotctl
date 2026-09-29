package ports

import "testing"

func TestAssign(t *testing.T) {
	got := Assign([]string{"web", "db"}, 12000, 3)
	if len(got) != 2 || got[0] != (Port{"web", 12200}) || got[1] != (Port{"db", 12201}) {
		t.Fatalf("Assign: %+v", got)
	}
	if m := Map(got); m["web"] != 12200 || m["db"] != 12201 {
		t.Fatalf("Map: %+v", m)
	}
}
