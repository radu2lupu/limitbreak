package main

import (
	"testing"
	"time"
)

func TestUsageReportListsEachAccountsHeaviestSessionsAndSumsTheRest(t *testing.T) {
	out := tokenUsage{OutputTokens: 1000} // 5k weighted
	records := []requestRecord{
		rec(30, "work", "a", nil, nil, out),
		rec(20, "work", "a", nil, nil, out),
		rec(10, "work", "b", nil, nil, out),
		rec(10, "home", "a", nil, nil, out), // a moved: counts on home too
		rec(9, ownAccount, "c", nil, nil, out),
		rec(8, "?", "d", nil, nil, out),
		rec(8*24*60, "work", "old", nil, nil, out), // before the period
	}
	for i := 0; i < usageReportSessions; i++ {
		records = append(records, rec(5, "work", string(rune('p'+i)), nil, nil, tokenUsage{InputTokens: 1}))
	}
	status := []accountStatus{{ID: "work", Provider: "claude"}, {ID: "home", Provider: "claude"}, {ID: "work", Provider: "codex"}}
	asked := 0
	info := func(id string) transcriptInfo { asked++; return transcriptInfo{Project: "~/p", Title: "title " + id} }
	report := buildUsageReport(status, records, 7*24*time.Hour, blameNow, info, func(string) string { return "" })
	if len(report.Accounts) != 3 {
		t.Fatalf("every provider's account should be listed: %+v", report.Accounts)
	}
	if cx := report.Accounts[2]; cx.Provider != "codex" || cx.Weighted != 0 || len(cx.Sessions) != 0 {
		t.Fatalf("a codex account sharing a claude id took its sessions: %+v", cx)
	}
	work := report.Accounts[0]
	if work.ID != "work" || work.Weighted != 15000+usageReportSessions || len(work.Sessions) != usageReportSessions {
		t.Fatalf("work: %+v", work)
	}
	if work.Sessions[0].Session != "a" || work.Sessions[0].Weighted != 10000 || work.Sessions[0].Title != "title a" {
		t.Fatalf("heaviest first: %+v", work.Sessions[0])
	}
	if work.Rest != 2 { // the two lightest of the five 1-token sessions
		t.Fatalf("rest %v", work.Rest)
	}
	if report.Accounts[1].Weighted != 5000 || report.Own != 5000 || report.Unrecorded != 5000 {
		t.Fatalf("home %v own %v unrecorded %v", report.Accounts[1].Weighted, report.Own, report.Unrecorded)
	}
	if asked != usageReportSessions+1 {
		t.Fatalf("transcripts asked %d times; only listed sessions should be", asked)
	}
}
