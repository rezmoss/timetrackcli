package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	binMinutes    = 5
	sampleSeconds = 30
	defaultFile   = "timetrackcli.json"
)

type Config struct {
	DailyGoalMinutes int   `json:"daily_goal_minutes"`
	WorkDays         []int `json:"work_days"` // 1=Monday, 7=Sunday
}

type Range struct {
	Start  int64 `json:"start"`
	End    int64 `json:"end"`
	Status int   `json:"status"`
}

type Store struct {
	Bins   map[string]int `json:"bins"`
	Ranges []Range        `json:"ranges"`
	Config Config         `json:"config"`
}

type dashboardModel struct {
	store    *Store
	filePath string
	width    int
	height   int
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#4A90E2")).
			Padding(0, 1).
			MarginBottom(1)

	workingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#04B575")).
			Bold(true)

	idleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF6B6B")).
			Bold(true)

	progressStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F7DC6F")).
			Bold(true)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#874BFD")).
			Padding(1, 2).
			MarginBottom(1)
)

type tickMsg time.Time

func compactBins(s *Store) {
	if len(s.Bins) < 50 {
		return
	}

	var times []time.Time
	for k := range s.Bins {
		if ts, err := strconv.ParseInt(k, 10, 64); err == nil {
			times = append(times, time.Unix(ts, 0))
		}
	}

	if len(times) == 0 {
		return
	}

	for i := 0; i < len(times)-1; i++ {
		for j := i + 1; j < len(times); j++ {
			if times[i].After(times[j]) {
				times[i], times[j] = times[j], times[i]
			}
		}
	}

	for i := 0; i < len(times); {
		start := times[i]
		status := s.Bins[strconv.FormatInt(start.Unix(), 10)]
		j := i

		for j < len(times)-1 {
			next := times[j+1]
			nextStatus := s.Bins[strconv.FormatInt(next.Unix(), 10)]
			if nextStatus == status && next.Sub(times[j]) == binMinutes*time.Minute {
				j++
			} else {
				break
			}
		}

		end := times[j].Add(binMinutes * time.Minute)
		s.Ranges = append(s.Ranges, Range{
			Start:  start.Unix(),
			End:    end.Unix(),
			Status: status,
		})

		i = j + 1
	}

	s.Bins = map[string]int{}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second*30, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m dashboardModel) Init() tea.Cmd {
	return tickCmd()
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		// Reload store data
		store, err := loadStore(m.filePath)
		if err == nil {
			m.store = store
		}
		return m, tickCmd()
	}
	return m, nil
}

func (m dashboardModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	now := time.Now()

	// Header - full width
	header := headerStyle.Width(m.width).Render(
		fmt.Sprintf("🕐 Time Tracker Dashboard - %s", now.Format("Jan 2, 2006 15:04:05")),
	)

	// Today's stats
	workMins, idleMins := todayTotals(m.store)
	totalMins := workMins + idleMins

	var workPct, idlePct float64
	if totalMins > 0 {
		workPct = float64(workMins) / float64(totalMins) * 100
		idlePct = float64(idleMins) / float64(totalMins) * 100
	}

	// Calculate column widths - use full terminal width
	leftColWidth := m.width/2 - 3
	rightColWidth := m.width/2 - 3

	var progressText string
	if isWorkDay(now, m.store.Config.WorkDays) {
		progressText = fmt.Sprintf("Progress: %s", progressStyle.Render(formatPercentage(workMins, m.store.Config.DailyGoalMinutes)))
	} else {
		progressText = "Progress: Weekend/Non-workday"
	}

	workingHoursBox := boxStyle.Width(leftColWidth).Render(fmt.Sprintf(
		"💼 WORKING HOURS\n\n"+
			"Working: %s\n"+
			"%s",
		workingStyle.Render(humanDuration(workMins)),
		progressText,
	))

	// Progress Bar Box
	goalPct := 0
	if m.store.Config.DailyGoalMinutes > 0 {
		goalPct = (workMins * 100) / m.store.Config.DailyGoalMinutes
	}
	progressBarWidth := leftColWidth - 10
	if progressBarWidth < 20 {
		progressBarWidth = 20
	}
	progressBar := createProgressBar(goalPct, progressBarWidth)

	progressBox := boxStyle.Width(leftColWidth).Render(fmt.Sprintf(
		"🎯 DAILY GOAL PROGRESS\n\n%s",
		func() string {
			if isWorkDay(now, m.store.Config.WorkDays) {
				return fmt.Sprintf("%s %d%%\n%s", progressBar, goalPct, progressStyle.Render(formatPercentage(workMins, m.store.Config.DailyGoalMinutes)))
			}
			return "No goal tracking on non-workdays"
		}(),
	))

	longestFocus, contextSwitches := calculateFocusStats(m.store)

	// Summary stats box
	summaryBox := boxStyle.Width(leftColWidth).Render(fmt.Sprintf(
		"📊 TODAY'S SUMMARY\n\n"+
			"Working: %s %s (%.1f%%)\n"+
			"Idle: %s %s (%.1f%%)\n"+
			"Total: %s\n\n"+
			"Longest Focus: %s\n"+
			"Context Switches: %s",
		workingStyle.Render("●"), humanDuration(workMins), workPct,
		idleStyle.Render("●"), humanDuration(idleMins), idlePct,
		humanDuration(totalMins),
		workingStyle.Render(humanDuration(longestFocus)),
		progressStyle.Render(fmt.Sprintf("%d", contextSwitches)),
	))

	// Live status
	var status string
	var statusColor lipgloss.Style
	if la, err := lastActivity(now); err == nil {
		idleSeconds := now.Sub(la).Seconds()
		if idleSeconds < 60 {
			status = "🟢 ACTIVE"
			statusColor = workingStyle
		} else {
			status = fmt.Sprintf("🔴 IDLE (%s)", humanDuration(int(idleSeconds/60)))
			statusColor = idleStyle
		}
	} else {
		status = "❓ UNKNOWN"
		statusColor = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500"))
	}

	liveBox := boxStyle.Width(leftColWidth).Render(fmt.Sprintf(
		"⚡ LIVE STATUS\n\n%s",
		statusColor.Render(status),
	))

	// Timeline box
	timelineBox := createTimelineBox(m.store, rightColWidth, m.height-8) // Reserve space for header/footer

	// 30-day grid box
	grid30Days := create30DayGrid(m.store, leftColWidth)
	gridBox := boxStyle.Width(leftColWidth).Render(grid30Days)

	// Best/Worst day box
	bestDay, bestMins, worstDay, worstMins := findBestWorstDays(m.store)
	bestWorstContent := "📈 BEST / WORST DAY (30 days)\n\n"
	if bestMins > 0 {
		bestWorstContent += fmt.Sprintf("Best: %s\n%s (%s)\n\n",
			workingStyle.Render("🏆"),
			bestDay.Format("Jan 2"),
			workingStyle.Render(humanDuration(bestMins)))
	} else {
		bestWorstContent += "Best: No work days found\n\n"
	}
	if worstMins < 9999 {
		bestWorstContent += fmt.Sprintf("Worst: %s\n%s (%s)",
			idleStyle.Render("📉"),
			worstDay.Format("Jan 2"),
			idleStyle.Render(humanDuration(worstMins)))
	} else {
		bestWorstContent += "Worst: No work days found"
	}
	bestWorstBox := boxStyle.Width(leftColWidth).Render(bestWorstContent)

	// Period Progress box
	weekHours, weekGoal, monthHours, monthGoal, yearHours, yearGoal := calculatePeriodProgress(m.store)
	periodContent := "🗓️  PERIOD GOALS\n\n"

	// Week progress
	weekPct := 0
	if weekGoal > 0 {
		weekPct = (weekHours * 100) / weekGoal
	}
	weekBar := createProgressBar(weekPct, leftColWidth-15)
	periodContent += fmt.Sprintf("Week: %s / %s\n%s %d%%\n\n",
		workingStyle.Render(humanDuration(weekHours)),
		progressStyle.Render(humanDuration(weekGoal)),
		weekBar, weekPct)

	// Month progress
	monthPct := 0
	if monthGoal > 0 {
		monthPct = (monthHours * 100) / monthGoal
	}
	monthBar := createProgressBar(monthPct, leftColWidth-15)
	periodContent += fmt.Sprintf("Month: %s / %s\n%s %d%%\n\n",
		workingStyle.Render(humanDuration(monthHours)),
		progressStyle.Render(humanDuration(monthGoal)),
		monthBar, monthPct)

	// Year progress
	yearPct := 0
	if yearGoal > 0 {
		yearPct = (yearHours * 100) / yearGoal
	}
	yearBar := createProgressBar(yearPct, leftColWidth-15)
	periodContent += fmt.Sprintf("Year: %s / %s\n%s %d%%",
		workingStyle.Render(humanDuration(yearHours)),
		progressStyle.Render(humanDuration(yearGoal)),
		yearBar, yearPct)

	periodBox := boxStyle.Width(leftColWidth).Render(periodContent)

	sevenDayBox := boxStyle.Width(leftColWidth).Render(create7DayWorkingHours(m.store, leftColWidth))

	// Layout with full width
	leftColumn := lipgloss.JoinVertical(lipgloss.Left, workingHoursBox, progressBox, summaryBox, sevenDayBox, gridBox, bestWorstBox, periodBox, liveBox)

	rightColumn := timelineBox

	content := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, rightColumn)

	footer := lipgloss.NewStyle().
		Width(m.width).
		Foreground(lipgloss.Color("#626262")).
		Render("Press 'q' or Ctrl+C to quit • Updates every 30 seconds")

	// Use full terminal height
	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		content,
		footer,
	)

	contentHeight := lipgloss.Height(fullContent)
	if contentHeight < m.height {
		padding := strings.Repeat("\n", m.height-contentHeight-1)
		fullContent += padding
	}

	return fullContent
}

func createProgressBar(percentage int, width int) string {
	if percentage > 100 {
		percentage = 100
	}
	filled := (percentage * width) / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Render(bar)
}

func createTimelineBox(s *Store, width, maxHeight int) string {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bins := fetchBins(s, start, now)

	// Create full sequence from midnight to now
	var seq []time.Time
	for cur := floorToBin(start); cur.Before(floorToBin(now)); cur = cur.Add(binMinutes * time.Minute) {
		seq = append(seq, cur)
	}

	// Initialize all bins as idle (0), then apply actual data
	status := map[time.Time]int{}
	for _, t := range seq {
		status[t] = 0
	}
	for t, v := range bins {
		status[t] = v
	}

	timeline := "📊 TODAY'S TIMELINE\n\n"

	// Build merged blocks
	var blocks []struct {
		start    time.Time
		end      time.Time
		status   int
		duration int
	}

	for i := 0; i < len(seq); {
		startBin := seq[i]
		st := status[startBin]
		j := i
		for j < len(seq) && status[seq[j]] == st {
			j++
		}
		endBin := seq[j-1].Add(binMinutes * time.Minute)
		duration := int(endBin.Sub(startBin).Minutes())

		blocks = append(blocks, struct {
			start    time.Time
			end      time.Time
			status   int
			duration int
		}{startBin, endBin, st, duration})

		i = j
	}

	// Calculate how many entries we can show based on available height
	maxEntries := maxHeight - 6
	if maxEntries < 5 {
		maxEntries = 5
	}

	// Show most recent blocks that fit in the height
	start_idx := 0
	if len(blocks) > maxEntries {
		start_idx = len(blocks) - maxEntries
	}

	for i := start_idx; i < len(blocks); i++ {
		block := blocks[i]

		var indicator, desc string
		var style lipgloss.Style
		if block.status == 1 {
			indicator = "🟢"
			desc = "working"
			style = workingStyle
		} else {
			indicator = "🔴"
			desc = "idle"
			style = idleStyle
		}

		timeRange := fmt.Sprintf("%s-%s", block.start.Format("15:04"), block.end.Format("15:04"))
		timeline += fmt.Sprintf("%s %s %s (%s)\n", indicator, timeRange, style.Render(desc), humanDuration(block.duration))
	}

	return boxStyle.Width(width).Height(maxHeight).Render(timeline)
}

func loadStore(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Store{
				Bins: map[string]int{},
				Config: Config{
					DailyGoalMinutes: 480,
					WorkDays:         []int{1, 2, 3, 4, 5},
				},
			}, nil
		}
		return nil, err
	}
	defer f.Close()
	var s Store
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	if s.Bins == nil {
		s.Bins = map[string]int{}
	}

	if s.Config.DailyGoalMinutes == 0 {
		s.Config.DailyGoalMinutes = 480 // 8 hours
	}
	if len(s.Config.WorkDays) == 0 {
		s.Config.WorkDays = []int{1, 2, 3, 4, 5} // Mon-Fri
	}

	return &s, nil
}

func saveStore(path string, s *Store) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func floorToBin(t time.Time) time.Time {
	t = t.Truncate(time.Minute)
	m := (t.Minute() / binMinutes) * binMinutes
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), m, 0, 0, t.Location())
}

func nextBinStart(t time.Time) time.Time { return floorToBin(t).Add(binMinutes * time.Minute) }

func humanDuration(mins int) string {
	h := mins / 60
	m := mins % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d hr %d mins", h, m)
	case h > 0:
		if h == 1 {
			return "1 hr"
		}
		return fmt.Sprintf("%d hrs", h)
	case m == 1:
		return "1 min"
	default:
		return fmt.Sprintf("%d mins", m)
	}
}

// macOS idle seconds via `ioreg -c IOHIDSystem`, parsing HIDIdleTime (nanoseconds since last input)
var hidIdleRe = regexp.MustCompile(`HIDIdleTime"\s*=\s*([0-9]+)`)

func getIdleSecondsMac() (float64, error) {
	cmd := exec.Command("/usr/sbin/ioreg", "-c", "IOHIDSystem")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "HIDIdleTime") {
			m := hidIdleRe.FindStringSubmatch(line)
			if len(m) == 2 {
				ns, _ := strconv.ParseFloat(m[1], 64)
				return ns / 1_000_000_000.0, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("HIDIdleTime not found")
}

func lastActivity(now time.Time) (time.Time, error) {
	idle, err := getIdleSecondsMac()
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-time.Duration(idle * float64(time.Second))), nil
}

func upsertBin(s *Store, binStart time.Time, working bool) {
	k := strconv.FormatInt(binStart.Unix(), 10)
	cur := s.Bins[k]
	if working && cur == 0 {
		s.Bins[k] = 1
	} else if cur == 0 && !working {
		if _, ok := s.Bins[k]; !ok {
			s.Bins[k] = 0
		}
	}
}

func fetchBins(s *Store, start, end time.Time) map[time.Time]int {
	res := make(map[time.Time]int)

	for k, v := range s.Bins {
		ts, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		t := time.Unix(ts, 0)
		if !t.Before(start) && t.Before(end) {
			res[t] = v
		}
	}

	for _, r := range s.Ranges {
		rStart := time.Unix(r.Start, 0)
		rEnd := time.Unix(r.End, 0)

		if rEnd.Before(start) || !rStart.Before(end) {
			continue
		}

		for cur := floorToBin(rStart); cur.Before(rEnd) && cur.Before(end); cur = cur.Add(binMinutes * time.Minute) {
			if !cur.Before(start) {
				res[cur] = r.Status
			}
		}
	}

	return res
}

func reportToday(s *Store) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := now
	bins := fetchBins(s, start, end)

	// build full sequence of bins and merge contiguous
	var seq []time.Time
	for cur := floorToBin(start); cur.Before(floorToBin(end)); cur = cur.Add(binMinutes * time.Minute) {
		seq = append(seq, cur)
	}
	status := map[time.Time]int{}
	for _, t := range seq {
		status[t] = 0
	}
	for t, v := range bins {
		status[t] = v
	}

	fmt.Println(now.Format("Date : Jan 2, 2006 , Monday"))
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("%-15s | %-12s | %s\n", "Time Range", "Duration", "Description")
	fmt.Println(strings.Repeat("-", 50))

	totalWork := 0
	for i := 0; i < len(seq); {
		startBin := seq[i]
		st := status[startBin]
		j := i
		for j < len(seq) && status[seq[j]] == st {
			j++
		}
		endBin := seq[j-1].Add(binMinutes * time.Minute)
		mins := int(endBin.Sub(startBin).Minutes())
		desc := "idle"
		if st == 1 {
			desc = "working"
			totalWork += mins
		}
		fmt.Printf("%s-%-7s | %-12s | %s\n", startBin.Format("15:04"), endBin.Format("15:04"), humanDuration(mins), desc)
		i = j
	}
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("Total working today : %s\n", humanDuration(totalWork))
	if isWorkDay(now, s.Config.WorkDays) {
		fmt.Printf("Daily goal progress: %s\n", formatPercentage(totalWork, s.Config.DailyGoalMinutes))
	}
}

func create7DayWorkingHours(s *Store, width int) string {
	now := time.Now()
	content := "📊 LAST 7 DAYS\n\n"

	totalWeekHours := 0

	for dayIndex := 0; dayIndex < 7; dayIndex++ {
		targetDay := now.AddDate(0, 0, -(6 - dayIndex)) // Start from 6 days ago to today

		// Get working minutes for this day
		dayStart := time.Date(targetDay.Year(), targetDay.Month(), targetDay.Day(), 0, 0, 0, 0, targetDay.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		bins := fetchBins(s, dayStart, dayEnd)

		workMins := 0
		for _, v := range bins {
			if v == 1 {
				workMins += binMinutes
			}
		}
		totalWeekHours += workMins

		// Format the day
		dayName := targetDay.Format("Mon")
		dateStr := targetDay.Format("Jan 2")

		// Color coding based on work hours and if it's a work day
		var dayStyle lipgloss.Style
		var indicator string

		isWork := isWorkDay(targetDay, s.Config.WorkDays)

		if workMins == 0 {
			dayStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#626262"))
			indicator = "⚫"
		} else if isWork && workMins >= s.Config.DailyGoalMinutes {
			dayStyle = workingStyle
			indicator = "✅"
		} else if workMins > 0 {
			dayStyle = progressStyle
			indicator = "🟡"
		} else {
			dayStyle = idleStyle
			indicator = "🔴"
		}

		// Format working hours
		hoursStr := humanDuration(workMins)
		if workMins == 0 {
			hoursStr = "No work"
		}

		content += fmt.Sprintf("%s %s %s: %s\n",
			indicator,
			dayName,
			dateStr,
			dayStyle.Render(hoursStr))
	}

	content += fmt.Sprintf("\nTotal: %s", workingStyle.Render(humanDuration(totalWeekHours)))

	return content
}

// Daily aggregate table used for week/month ranges
func reportAggregateDaily(s *Store, start time.Time, days int, title string) {
	fmt.Println(title)
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("%-15s | %s\n", "Date", "Working Time")
	fmt.Println(strings.Repeat("-", 50))
	total := 0
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		dayStart := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		bins := fetchBins(s, dayStart, dayEnd)
		mins := 0
		for _, v := range bins {
			if v == 1 {
				mins += binMinutes
			}
		}
		total += mins
		fmt.Printf("%-15s | %s\n", d.Format("2006-01-02"), humanDuration(mins))
	}
	fmt.Println(strings.Repeat("-", 50))
	lower := strings.ToLower(title)
	noun := "range"
	if strings.Contains(lower, "week") {
		noun = "week"
	}
	if strings.Contains(lower, "month") {
		noun = "month"
	}
	fmt.Printf("Total working %s : %s\n", noun, humanDuration(total))
	workDaysInRange := 0
	for i := 0; i < days; i++ {
		if isWorkDay(start.AddDate(0, 0, i), s.Config.WorkDays) {
			workDaysInRange++
		}
	}
	if workDaysInRange > 0 {
		expectedMins := workDaysInRange * s.Config.DailyGoalMinutes
		fmt.Printf("Goal progress: %s\n", formatPercentage(total, expectedMins))
	}

}

// Year report: monthly totals
func reportYearMonthly(s *Store, year int) {
	loc := time.Now().Location()
	fmt.Printf("for year %d (monthly totals)\n", year)
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("%-15s | %s\n", "Month", "Working Time")
	fmt.Println(strings.Repeat("-", 50))
	total := 0
	for m := time.January; m <= time.December; m++ {
		start := time.Date(year, m, 1, 0, 0, 0, 0, loc)
		next := start.AddDate(0, 1, 0)
		bins := fetchBins(s, start, next)
		mins := 0
		for _, v := range bins {
			if v == 1 {
				mins += binMinutes
			}
		}
		total += mins
		fmt.Printf("%-15s | %s\n", start.Format("Jan"), humanDuration(mins))
	}
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("Total working year : %s\n", humanDuration(total))
	workDaysInYear := 0
	for m := time.January; m <= time.December; m++ {
		start := time.Date(year, m, 1, 0, 0, 0, 0, loc)
		next := start.AddDate(0, 1, 0)
		for d := start; d.Before(next); d = d.AddDate(0, 0, 1) {
			if isWorkDay(d, s.Config.WorkDays) {
				workDaysInYear++
			}
		}
	}
	expectedMins := workDaysInYear * s.Config.DailyGoalMinutes
	fmt.Printf("Goal progress: %s\n", formatPercentage(total, expectedMins))
}

func report(s *Store, rng string) {
	now := time.Now()
	switch rng {
	case "today":
		reportToday(s)
	case "week":
		// ISO week: Monday start
		weekday := int(now.Weekday())
		if weekday == 0 { // Sunday -> 7
			weekday = 7
		}
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(weekday - 1))
		reportAggregateDaily(s, start, 7, fmt.Sprintf("for week starting %s", start.Format("2006-01-02")))
	case "month":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		next := start.AddDate(0, 1, 0)
		days := int(next.Sub(start).Hours() / 24)
		reportAggregateDaily(s, start, days, fmt.Sprintf("for month %s", start.Format("2006-01")))
	case "year":
		reportYearMonthly(s, now.Year())
	default:
		fmt.Printf("Unknown range '%s'\n", rng)
	}
}

func todayTotals(s *Store) (workMins, idleMins int) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := now
	var seq []time.Time
	for cur := floorToBin(start); cur.Before(floorToBin(end)); cur = cur.Add(binMinutes * time.Minute) {
		seq = append(seq, cur)
	}
	status := map[time.Time]int{}
	for _, t := range seq {
		status[t] = 0
	}
	bins := fetchBins(s, start, end)
	for t, v := range bins {
		status[t] = v
	}
	for _, t := range seq {
		if status[t] == 1 {
			workMins += binMinutes
		} else {
			idleMins += binMinutes
		}
	}
	return
}

func ensureStartupAtLogin(execPath string) {
	usr, err := user.Current()
	if err != nil {
		return
	}
	uid := usr.Uid
	base := strings.TrimSuffix(filepath.Base(execPath), filepath.Ext(execPath))
	base = strings.ToLower(strings.ReplaceAll(base, " ", "-"))
	label := "com." + base + ".autostart"

	agentsDir := filepath.Join(usr.HomeDir, "Library", "LaunchAgents")
	plistPath := filepath.Join(agentsDir, label+".plist")

	installed := false
	if _, err := os.Stat(plistPath); err == nil {
		if err := exec.Command("launchctl", "print", "gui/"+uid+"/"+label).Run(); err == nil {
			installed = true
		}
	}
	if installed {
		return
	}

	fmt.Print("[startup] This app is not set to launch at login. Add it now? [y/N]: ")
	rd := bufio.NewReader(os.Stdin)
	ans, _ := rd.ReadString('\n')
	ans = strings.TrimSpace(strings.ToLower(ans))
	if ans != "y" && ans != "yes" {
		fmt.Println("[startup] Skipping adding to startup.")
		return
	}

	_ = os.MkdirAll(agentsDir, 0755)
	outLog := filepath.Join(agentsDir, label+".out.log")
	errLog := filepath.Join(agentsDir, label+".err.log")
	dataFile := filepath.Join(filepath.Dir(execPath), "timetrackcli.json")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>--file</string><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>WorkingDirectory</key><string>%s</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, label, execPath, dataFile, filepath.Dir(execPath), outLog, errLog)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fmt.Println("[startup] Failed to write LaunchAgent:", err)
		return
	}

	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).Run(); err != nil {
		_ = exec.Command("launchctl", "load", "-w", plistPath).Run()
	}
	_ = exec.Command("launchctl", "enable", "gui/"+uid+"/"+label).Run()
	_ = exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+label).Run()
	fmt.Println("[startup] Added to login (LaunchAgents):", plistPath)
}

func parseTimeToMinutes(timeStr string) (int, error) {
	parts := strings.Split(timeStr, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time format, use HH:MM")
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	mins, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	return hours*60 + mins, nil
}

func parseWorkDays(workDaysStr string) ([]int, error) {
	dayMap := map[string]int{
		"mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6, "sun": 7,
	}

	var days []int
	if strings.Contains(workDaysStr, "-") {
		parts := strings.Split(workDaysStr, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid range format")
		}
		start, ok1 := dayMap[strings.ToLower(parts[0])]
		end, ok2 := dayMap[strings.ToLower(parts[1])]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid day names")
		}
		for i := start; i <= end; i++ {
			days = append(days, i)
		}
	} else {
		parts := strings.Split(workDaysStr, ",")
		for _, part := range parts {
			day, ok := dayMap[strings.ToLower(strings.TrimSpace(part))]
			if !ok {
				return nil, fmt.Errorf("invalid day name: %s", part)
			}
			days = append(days, day)
		}
	}
	return days, nil
}

func formatPercentage(workMins, goalMins int) string {
	if goalMins == 0 {
		return "0%"
	}
	pct := (workMins * 100) / goalMins
	return fmt.Sprintf("%d%% of %s", pct, humanDuration(goalMins))
}

func isWorkDay(t time.Time, workDays []int) bool {
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	for _, day := range workDays {
		if day == weekday {
			return true
		}
	}
	return false
}

func findBestWorstDays(s *Store) (bestDay time.Time, bestMins int, worstDay time.Time, worstMins int) {
	now := time.Now()
	bestMins = -1
	worstMins = 9999

	for dayIndex := 0; dayIndex < 30; dayIndex++ {
		targetDay := now.AddDate(0, 0, -(29 - dayIndex))

		dayStart := time.Date(targetDay.Year(), targetDay.Month(), targetDay.Day(), 0, 0, 0, 0, targetDay.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		bins := fetchBins(s, dayStart, dayEnd)

		workMins := 0
		for _, v := range bins {
			if v == 1 {
				workMins += binMinutes
			}
		}

		// Skip days with no work
		if workMins == 0 {
			continue
		}

		// Check for best day
		if workMins > bestMins {
			bestMins = workMins
			bestDay = targetDay
		}

		// Check for worst day (but not zero)
		if workMins < worstMins {
			worstMins = workMins
			worstDay = targetDay
		}
	}

	return
}

func create30DayGrid(s *Store, width int) string {
	now := time.Now()
	grid := "📅 LAST 30 DAYS\n\n"

	line := ""
	for dayIndex := 0; dayIndex < 30; dayIndex++ {
		targetDay := now.AddDate(0, 0, -(29 - dayIndex))

		// Get working minutes for this day
		dayStart := time.Date(targetDay.Year(), targetDay.Month(), targetDay.Day(), 0, 0, 0, 0, targetDay.Location())
		dayEnd := dayStart.Add(24 * time.Hour)
		bins := fetchBins(s, dayStart, dayEnd)

		workMins := 0
		for _, v := range bins {
			if v == 1 {
				workMins += binMinutes
			}
		}

		// Determine symbol based on work hours
		var symbol string

		if workMins == 0 {
			symbol = "⚫" // Gray circle - no data
		} else if workMins < 120 { // Less than 2 hours
			symbol = "⚪" // White circle
		} else if workMins <= 300 { // 2-5 hours
			symbol = "🟡" // Yellow circle
		} else { // Above 5 hours
			symbol = "🟢" // Green circle
		}

		// Use checkmark if it's a workday and meets goal
		if isWorkDay(targetDay, s.Config.WorkDays) && workMins >= s.Config.DailyGoalMinutes {
			symbol = "✅"
		}

		line += symbol
	}

	grid += line + "\n\n"
	grid += "⚫ No data  ⚪ <2hrs  🟡 2-5hrs  🟢 >5hrs  ✅ Goal met"
	return grid
}

func calculateFocusStats(s *Store) (longestFocus int, contextSwitches int) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bins := fetchBins(s, start, now)

	// Create full sequence from midnight to now
	var seq []time.Time
	for cur := floorToBin(start); cur.Before(floorToBin(now)); cur = cur.Add(binMinutes * time.Minute) {
		seq = append(seq, cur)
	}

	status := map[time.Time]int{}
	for _, t := range seq {
		status[t] = 0
	}
	for t, v := range bins {
		status[t] = v
	}

	if len(seq) == 0 {
		return 0, 0
	}

	// Find longest working session and count context switches
	longestFocus = 0
	currentFocus := 0
	prevStatus := status[seq[0]]

	for _, t := range seq {
		currentStatus := status[t]

		if currentStatus == 1 { // Working
			currentFocus += binMinutes
			if currentFocus > longestFocus {
				longestFocus = currentFocus
			}
		} else { // Idle
			currentFocus = 0
		}

		// Count context switches (status changes)
		if currentStatus != prevStatus {
			contextSwitches++
		}
		prevStatus = currentStatus
	}

	return longestFocus, contextSwitches
}

func calculatePeriodProgress(s *Store) (weekHours, weekGoal, monthHours, monthGoal, yearHours, yearGoal int) {
	now := time.Now()

	// Week calculation (ISO week: Monday start)
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	weekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(weekday - 1))
	weekEnd := weekStart.AddDate(0, 0, 7)

	// Month calculation
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)

	// Year calculation
	yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
	yearEnd := yearStart.AddDate(1, 0, 0)

	// Calculate week hours and goal
	weekBins := fetchBins(s, weekStart, weekEnd)
	for _, v := range weekBins {
		if v == 1 {
			weekHours += binMinutes
		}
	}
	for d := weekStart; d.Before(weekEnd); d = d.AddDate(0, 0, 1) {
		if isWorkDay(d, s.Config.WorkDays) {
			weekGoal += s.Config.DailyGoalMinutes
		}
	}

	// Calculate month hours and goal
	monthBins := fetchBins(s, monthStart, monthEnd)
	for _, v := range monthBins {
		if v == 1 {
			monthHours += binMinutes
		}
	}
	for d := monthStart; d.Before(monthEnd); d = d.AddDate(0, 0, 1) {
		if isWorkDay(d, s.Config.WorkDays) {
			monthGoal += s.Config.DailyGoalMinutes
		}
	}

	// Calculate year hours and goal
	yearBins := fetchBins(s, yearStart, yearEnd)
	for _, v := range yearBins {
		if v == 1 {
			yearHours += binMinutes
		}
	}
	for d := yearStart; d.Before(yearEnd); d = d.AddDate(0, 0, 1) {
		if isWorkDay(d, s.Config.WorkDays) {
			yearGoal += s.Config.DailyGoalMinutes
		}
	}

	return
}

func main() {
	reportFlag := flag.Bool("report", false, "print report and exit")
	rng := flag.String("range", "today", "report range: today|week|month|year")
	file := flag.String("file", defaultFile, "path to JSON store")
	configFlag := flag.String("config", "", "config in format key=value (e.g., dailygoal=07:30 or workdays=Mon-Fri)")
	dashboardFlag := flag.Bool("dashboard", false, "show interactive dashboard")

	flag.Parse()

	if *configFlag != "" {
		parts := strings.SplitN(*configFlag, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "Invalid config format. Use key=value")
			os.Exit(1)
		}

		store, err := loadStore(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load store:", err)
			os.Exit(1)
		}

		switch parts[0] {
		case "dailygoal":
			mins, err := parseTimeToMinutes(parts[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "Invalid time format:", err)
				os.Exit(1)
			}
			store.Config.DailyGoalMinutes = mins
		case "workdays":
			days, err := parseWorkDays(parts[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "Invalid workdays format:", err)
				os.Exit(1)
			}
			store.Config.WorkDays = days
		default:
			fmt.Fprintln(os.Stderr, "Unknown config key:", parts[0])
			os.Exit(1)
		}

		if err := saveStore(*file, store); err != nil {
			fmt.Fprintln(os.Stderr, "save config:", err)
			os.Exit(1)
		}
		fmt.Printf("Config updated: %s=%s\n", parts[0], parts[1])
		return
	}

	if execPath, err := os.Executable(); err == nil {
		ensureStartupAtLogin(execPath)
	}

	if dir := filepath.Dir(*file); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil && !os.IsExist(err) {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
	}

	store, err := loadStore(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load store:", err)
		os.Exit(1)
	}

	if *dashboardFlag {
		m := dashboardModel{
			store:    store,
			filePath: *file,
		}
		p := tea.NewProgram(m, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running dashboard: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *reportFlag {
		report(store, *rng)
		return
	}

	fmt.Println("[timetracking] Tracking started. Ctrl+C to stop.")
	for {
		now := time.Now()
		currentBin := floorToBin(now)
		if la, err := lastActivity(now); err == nil {
			working := !la.Before(currentBin) // last activity >= bin start
			upsertBin(store, currentBin, working)
			_ = saveStore(*file, store)

			if len(store.Bins) > 100 {
				compactBins(store)
				_ = saveStore(*file, store)
			}
		}
		w, i := todayTotals(store)
		fmt.Printf("[status] working: %s | idle: %s\r", humanDuration(w), humanDuration(i))
		time.Sleep(sampleSeconds * time.Second)
	}
}
