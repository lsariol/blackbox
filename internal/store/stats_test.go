package store

import (
	"context"
	"testing"
	"time"
)

func TestFingerprint(t *testing.T) {
	s := open(t)
	cases := []struct{ message, attrs, want string }{
		{"database: query failed", "", "database: query failed"},
		{"retrying in 1500ms (attempt 3 of 5)", "", "retrying in <n>ms (attempt <n> of <n>)"},
		{"took 1.25s on v1.2.3", "", "took <n>s on v<n>"},
		{"request 3f2a9c1e-77b0-4c1d-9e2f-0a1b2c3d4e5f failed", "", "request <id> failed"},
		{"container 4f1c2a9b8e7d started", "", "container <id> started"},
		{"commit ab12cd3 and job req_8fK2xQ9z", "", "commit <id> and job <id>"},
		{"refused 15000ms after 10.0.0.5:5432", "", "refused <n>ms after <n>:<n>"},
		{"sending failed", `{"err":"Twitch: 429 after 3 tries"}`, "sending failed err=Twitch: <n> after <n> tries"},
		{"sending failed", `{"other":"x"}`, "sending failed"},
	}
	for _, c := range cases {
		var attrs any
		if c.attrs != "" {
			attrs = c.attrs
		}
		var got string
		err := s.pool.QueryRow(context.Background(),
			"SELECT "+fingerprintSQL+" FROM (SELECT $1::text AS message, $2::text::jsonb AS attrs) r", c.message, attrs).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%q, %s:\n got %q\nwant %q", c.message, c.attrs, got, c.want)
		}
	}
}

func TestErrorCounts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lys := add(t, s, "lys", "lys", "lys", "v1")
	sonar := add(t, s, "sonar", "sonar", "sonar-api", "v1")
	write(t, s, lys, day, 40, "a", "b")                    // 12:00
	write(t, s, lys, day.Add(10*time.Minute), 30, "c")     // 12:10
	write(t, s, lys, day.Add(time.Hour), 20, "info")       // not counted
	write(t, s, lys, day.Add(time.Hour), 0, "no level")    // not counted
	write(t, s, sonar, day.Add(90*time.Minute), 40, "d")   // 13:30
	write(t, s, sonar, day.Add(-13*time.Hour), 30, "late") // 23:00 the day before

	db := reader(t)
	got, err := db.ErrorCounts(ctx, day.Add(-24*time.Hour), day.Add(2*time.Hour), BucketHour, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []ErrorCount{
		{Start: day.Add(-13 * time.Hour), Project: "sonar", Warns: 1},
		{Start: day, Project: "lys", Errors: 2, Warns: 1},
		{Start: day.Add(time.Hour), Project: "sonar", Errors: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("hours = %+v", got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || got[i].Project != want[i].Project ||
			got[i].Errors != want[i].Errors || got[i].Warns != want[i].Warns {
			t.Errorf("hour %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	days, err := db.ErrorCounts(ctx, day.Add(-24*time.Hour), day.Add(2*time.Hour), BucketDay, []string{"sonar"})
	if err != nil {
		t.Fatal(err)
	}
	midnight := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if len(days) != 2 || !days[0].Start.Equal(midnight.Add(-24*time.Hour)) || !days[1].Start.Equal(midnight) ||
		days[1].Errors != 1 || days[0].Warns != 1 {
		t.Errorf("sonar's days = %+v", days)
	}
}

func TestErrorGroups(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lys := add(t, s, "lys", "lys", "lys", "v1")
	sonar := add(t, s, "sonar", "sonar", "sonar-api", "v1")
	write(t, s, lys, day, 40, "query failed after 120ms", "query failed after 95ms", "query failed after 3000ms")
	write(t, s, lys, day.Add(time.Minute), 40, "panic: nil map")
	write(t, s, lys, day.Add(2*time.Minute), 30, "query failed after 1ms") // a warn: its own group
	write(t, s, sonar, day, 40, "query failed after 7ms")                  // another project: its own group
	b := Batch{Records: []Record{
		{ContainerID: lys.ID, Time: day.Add(3 * time.Minute), Stream: 2, Level: 40, Message: "send failed", Attrs: []byte(`{"err":"Twitch: 429"}`)},
		{ContainerID: lys.ID, Time: day.Add(4 * time.Minute), Stream: 2, Level: 40, Message: "send failed", Attrs: []byte(`{"err":"Twitch: 500"}`)},
		{ContainerID: lys.ID, Time: day.Add(5 * time.Minute), Stream: 2, Level: 40, Message: "send failed", Attrs: []byte(`{"err":"Twitch: timeout"}`)},
	}}
	if err := s.Save(ctx, b, day); err != nil {
		t.Fatal(err)
	}

	db := reader(t)
	q := GroupsQuery{From: day.Add(-time.Hour), To: day.Add(time.Hour), MinLevel: 40, Limit: 10}
	groups, total, err := db.ErrorGroups(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(groups) != 5 {
		t.Fatalf("total %d, groups %+v", total, groups)
	}
	top := groups[0]
	if top.Fingerprint != "query failed after <n>ms" || top.Project != "lys" || top.Count != 3 ||
		!top.FirstSeen.Equal(day) || !top.LastSeen.Equal(day.Add(2*time.Second)) ||
		top.NewestMessage != "query failed after 3000ms" {
		t.Errorf("top group = %+v", top)
	}
	second := groups[1]
	if second.Fingerprint != "send failed err=Twitch: <n>" || second.Count != 2 {
		t.Errorf("second group = %+v", second)
	}
	newest, err := db.Record(ctx, second.NewestID)
	if err != nil || !newest.Time.Equal(day.Add(4*time.Minute)) {
		t.Errorf("the newest line of %q is %+v, %v", second.Fingerprint, newest, err)
	}

	q.Limit, q.MinLevel, q.Projects = 2, 30, []string{"lys"}
	groups, total, err = db.ErrorGroups(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(groups) != 2 { // the warn is a group of its own
		t.Errorf("lys with warns, 2 at most: total %d, groups %+v", total, groups)
	}
}
