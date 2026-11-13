package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type usageValue struct {
	Value    float64   `json:"value"`
	Observed time.Time `json:"observed"`
}
type usageRecord struct {
	Email  string                `json:"email"`
	Values map[string]usageValue `json:"values"`
}

var usageHeaders = map[string]string{
	"five_hour_utilization": "Anthropic-Ratelimit-Unified-5h-Utilization",
	"five_hour_reset":       "Anthropic-Ratelimit-Unified-5h-Reset",
	"seven_day_utilization": "Anthropic-Ratelimit-Unified-7d-Utilization",
	"seven_day_reset":       "Anthropic-Ratelimit-Unified-7d-Reset",
}

func usagePath(home string) string {
	return filepath.Join(home, ".local", "state", "limitbreak", "usage.json")
}
func readUsage(home string) (map[string]usageRecord, error) {
	records := map[string]usageRecord{}
	raw, err := os.ReadFile(usagePath(home))
	if os.IsNotExist(err) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &records); err != nil {
		return nil, err
	}
	if records == nil {
		records = map[string]usageRecord{}
	}
	return records, nil
}

func (a *claudeAccounts) recordUsage(acc account, h http.Header) {
	values := map[string]usageValue{}
	for name, header := range usageHeaders {
		value, err := strconv.ParseFloat(h.Get(header), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		if (name == "five_hour_utilization" || name == "seven_day_utilization") && value > 1 {
			continue
		}
		values[name] = usageValue{Value: value, Observed: a.now().UTC()}
	}
	if len(values) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	records, err := readUsage(a.home)
	if err != nil {
		log.Printf("usage record read: %v", err)
		return
	}
	record := records[acc.ID]
	if record.Email != acc.Email || record.Values == nil {
		record = usageRecord{Email: acc.Email, Values: map[string]usageValue{}}
	}
	for name, value := range values {
		record.Values[name] = value
	}
	records[acc.ID] = record
	raw, err := json.Marshal(records)
	if err != nil {
		log.Printf("usage record encode: %v", err)
		return
	}
	dir := filepath.Dir(usagePath(a.home))
	if err = os.MkdirAll(dir, 0700); err != nil {
		log.Printf("usage record directory: %v", err)
		return
	}
	f, err := os.CreateTemp(dir, ".usage-*")
	if err != nil {
		log.Printf("usage record create: %v", err)
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), usagePath(a.home))
	}
	if err != nil {
		log.Printf("usage record write: %v", err)
	}
}

// usageFor is the account's recorded usage values; nil when there are none, or when they were
// recorded for another login under the same id.
func usageFor(records map[string]usageRecord, acc account) map[string]usageValue {
	record, ok := records[acc.ID]
	if !ok || record.Email != acc.Email || len(record.Values) == 0 {
		return nil
	}
	return record.Values
}

func showUsage(out io.Writer, home string, acc account) {
	records, err := readUsage(home)
	if err != nil {
		fmt.Fprintf(out, "    usage record unavailable: %v\n", err)
		return
	}
	values := usageFor(records, acc)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		item := values[name]
		value := fmt.Sprintf("%.1f%% used", item.Value*100)
		if name == "five_hour_reset" || name == "seven_day_reset" {
			value = time.Unix(int64(item.Value), 0).Local().Format(time.RFC1123)
		}
		fmt.Fprintf(out, "    %s: %s (observed %s)\n", name, value, item.Observed.Local().Format(time.RFC1123))
	}
}
