package usagemetering

import "testing"

func TestMonthlySchedulePeriodAt(t *testing.T) {
	s, err := NewMonthlySchedule(1, 0, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		at   string
		want string
	}{
		{"first day", "2026-09-01T00:00:00Z", "2026-09"},
		{"mid month", "2026-09-15T12:00:00Z", "2026-09"},
		{"last instant before boundary", "2026-09-30T23:59:59Z", "2026-09"},
		{"boundary belongs to next", "2026-10-01T00:00:00Z", "2026-10"},
		{"year wrap", "2026-12-31T23:59:59Z", "2026-12"},
		{"new year", "2027-01-01T00:00:00Z", "2027-01"},
	}
	for _, c := range cases {
		p := s.PeriodAt(mustParseTime(c.at))
		if p.ID != c.want {
			t.Errorf("%s: PeriodAt(%s).ID = %q, want %q", c.name, c.at, p.ID, c.want)
		}
	}
}

func TestMonthlyScheduleBounds(t *testing.T) {
	s, _ := NewMonthlySchedule(1, 0, 0, 0, nil)
	sep, err := s.PeriodByID("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !sep.Start.Equal(mustParseTime("2026-09-01T00:00:00Z")) ||
		!sep.End.Equal(mustParseTime("2026-10-01T00:00:00Z")) {
		t.Errorf("Sep bounds wrong: %s .. %s", sep.Start, sep.End)
	}
	if !sep.Contains(mustParseTime("2026-09-01T00:00:00Z")) {
		t.Error("start should be contained")
	}
	if sep.Contains(mustParseTime("2026-10-01T00:00:00Z")) {
		t.Error("end should be excluded")
	}
	if n := s.Next(sep); n.ID != "2026-10" {
		t.Errorf("next = %q", n.ID)
	}
}

func TestMonthlyScheduleCustomCutoffClamped(t *testing.T) {
	// 每月 31 日截点：2 月没有 31 日，应钳制到月末。
	s, err := NewMonthlySchedule(31, 12, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	feb, err := s.PeriodByID("2026-02")
	if err != nil {
		t.Fatal(err)
	}
	// 2026 非闰年，2 月 28 天。
	if want := mustParseTime("2026-02-28T12:00:00Z"); !feb.Start.Equal(want) {
		t.Errorf("Feb start = %s, want %s", feb.Start, want)
	}
	if want := mustParseTime("2026-03-31T12:00:00Z"); !feb.End.Equal(want) {
		t.Errorf("Feb end = %s, want %s", feb.End, want)
	}
	// 2 月周期内的时刻。
	if p := s.PeriodAt(mustParseTime("2026-03-15T00:00:00Z")); p.ID != "2026-02" {
		t.Errorf("Mar 15 with 31-day cutoff belongs to %q, want 2026-02", p.ID)
	}
}

func TestMonthlyScheduleInvalid(t *testing.T) {
	if _, err := NewMonthlySchedule(0, 0, 0, 0, nil); err == nil {
		t.Error("day=0 should be invalid")
	}
	if _, err := NewMonthlySchedule(32, 0, 0, 0, nil); err == nil {
		t.Error("day=32 should be invalid")
	}
	if _, err := NewMonthlySchedule(1, 24, 0, 0, nil); err == nil {
		t.Error("hour=24 should be invalid")
	}
	s, _ := NewMonthlySchedule(1, 0, 0, 0, nil)
	for _, id := range []string{"", "2026", "2026-9", "2026-13", "abcd-ef"} {
		if _, err := s.PeriodByID(id); err == nil {
			t.Errorf("PeriodByID(%q) should fail", id)
		}
	}
}
