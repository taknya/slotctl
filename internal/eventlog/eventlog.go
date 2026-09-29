// Package eventlog は、出来事の記録（events-YYYY-MM.jsonl）を追記し、古い記録を消す。
package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Event は、記録の1行である。
type Event struct {
	At             string `json:"at"`
	Event          string `json:"event"`
	Project        string `json:"project"`
	Slot           int    `json:"slot,omitempty"`
	Holder         string `json:"holder"`
	PreviousHolder string `json:"previous_holder,omitempty"`
	OK             *bool  `json:"ok,omitempty"`
	MS             *int64 `json:"ms,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Log は、記録の置き場である。
type Log struct {
	Dir             string
	Project         string
	RetentionMonths int
	Now             func() time.Time
}

var filePattern = regexp.MustCompile(`^events-(\d{4})-(\d{2})\.jsonl$`)

func monthIndex(year, month int) int { return year*12 + month - 1 }

// Record は、eventを追記する。書くときに、古い記録を消す。
func (l *Log) Record(ev Event) error {
	// 借り手のpathを持つので、同じmachineの他の利用者から読めないようにする
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	now := l.Now()
	Prune(l.Dir, l.RetentionMonths, now)
	ev.At = now.Format(time.RFC3339)
	ev.Project = l.Project
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	name := "events-" + now.Format("2006-01") + ".jsonl"
	f, err := os.OpenFile(filepath.Join(l.Dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Prune は、今月を含む直近retentionMonthsか月より古いevents-*.jsonlを消す。
func Prune(dir string, retentionMonths int, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	oldest := monthIndex(now.Year(), int(now.Month())) - (retentionMonths - 1)
	for _, e := range entries {
		m := filePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		if mo < 1 || mo > 12 {
			continue
		}
		if monthIndex(y, mo) < oldest {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// BoolPtr・MS は、任意項目の値を作る。
func BoolPtr(b bool) *bool { return &b }

func MS(d time.Duration) *int64 { v := d.Milliseconds(); return &v }
