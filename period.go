package usagemetering

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var periodIDPattern = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

func parsePeriodID(id string) (int, int) {
	y, _ := strconv.Atoi(id[:4])
	m, _ := strconv.Atoi(id[5:7])
	return y, m
}

// Period 表示一个结算周期，时间区间为左闭右开 [Start, End)。
type Period struct {
	ID    string
	Start time.Time
	End   time.Time
}

// Contains 报告 t 是否落在本周期内。
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.Start) && t.Before(p.End)
}

// Schedule 决定时间点到结算周期的映射，以及每月的结算截点。
type Schedule interface {
	// PeriodAt 返回 t 所在周期。
	PeriodAt(t time.Time) Period
	// PeriodByID 按 ID（如月结方案的 "2026-09"）解析周期。
	PeriodByID(id string) (Period, error)
	// Next 返回 p 之后紧邻的周期。
	Next(p Period) Period
}

// MonthlySchedule 月结方案：每个自然月的指定日、指定时刻为结算截点。
// 例如截点为每月 1 日 00:00 时，"2026-09" 周期覆盖
// [2026-09-01 00:00, 2026-10-01 00:00)。
// 当截点日超过某月天数时（如 31 日遇到 2 月），截点落到该月最后一天
// 同一时刻。
type MonthlySchedule struct {
	day  int
	hour int
	min  int
	sec  int
	loc  *time.Location
}

// NewMonthlySchedule 创建月结方案。day 为截点日（1..31），
// hour/min/sec 为截点时刻，loc 为 nil 时使用 UTC。
func NewMonthlySchedule(day, hour, min, sec int, loc *time.Location) (*MonthlySchedule, error) {
	if day < 1 || day > 31 {
		return nil, newError(KindValidation, "cutoff day must be in 1..31, got %d", day)
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 || sec < 0 || sec > 59 {
		return nil, newError(KindValidation, "invalid cutoff time %02d:%02d:%02d", hour, min, sec)
	}
	if loc == nil {
		loc = time.UTC
	}
	return &MonthlySchedule{day: day, hour: hour, min: min, sec: sec, loc: loc}, nil
}

// boundary 返回 y 年 m 月的截点时刻。
func (s *MonthlySchedule) boundary(y int, m time.Month) time.Time {
	// 下个月第 0 天即本月最后一天，用来做天数钳制。
	lastDay := time.Date(y, m+1, 0, 0, 0, 0, 0, s.loc).Day()
	d := s.day
	if d > lastDay {
		d = lastDay
	}
	return time.Date(y, m, d, s.hour, s.min, s.sec, 0, s.loc)
}

// PeriodAt 实现 Schedule。
func (s *MonthlySchedule) PeriodAt(t time.Time) Period {
	t = t.In(s.loc)
	y, m := t.Year(), t.Month()
	start := s.boundary(y, m)
	if t.Before(start) {
		// 本月截点尚未到来，归属从上月截点开始的周期。
		start = s.boundary(y, m-1)
	}
	end := s.boundary(start.Year(), start.Month()+1)
	return Period{
		ID:    fmt.Sprintf("%04d-%02d", start.Year(), start.Month()),
		Start: start,
		End:   end,
	}
}

// PeriodByID 实现 Schedule。id 形如 "2026-09"。
func (s *MonthlySchedule) PeriodByID(id string) (Period, error) {
	if !periodIDPattern.MatchString(id) {
		return Period{}, newError(KindValidation, "invalid period id %q, want YYYY-MM", id)
	}
	y, m := parsePeriodID(id)
	start := s.boundary(y, time.Month(m))
	end := s.boundary(y, time.Month(m)+1)
	return Period{ID: id, Start: start, End: end}, nil
}

// Next 实现 Schedule。
func (s *MonthlySchedule) Next(p Period) Period {
	return s.PeriodAt(p.End)
}
