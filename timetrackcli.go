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
)

const (
	binMinutes    = 5
	sampleSeconds = 30
	defaultFile   = "timetrackcli.json"
)

type Store struct {
	Bins map[string]int `json:"bins"`
}

func loadStore(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Store{Bins: map[string]int{}}, nil
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
		s.Bins[k] = 1 // once working, keep it working
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

func main() {
	reportFlag := flag.Bool("report", false, "print report and exit")
	rng := flag.String("range", "today", "report range: today|week|month|year")
	file := flag.String("file", defaultFile, "path to JSON store")
	flag.Parse()

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
		}
		w, i := todayTotals(store)
		fmt.Printf("[status] working: %s | idle: %s\r", humanDuration(w), humanDuration(i))
		time.Sleep(sampleSeconds * time.Second)
	}
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
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, label, execPath, outLog, errLog)

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
