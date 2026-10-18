package capacity

import (
	"errors"
	"strings"
	"testing"
)

// fakeProbe is a deterministic Probe for tests.
type fakeProbe struct {
	stats    Stats
	statsErr error

	fs    map[string]FSStats
	fsErr error
}

func (f fakeProbe) Stats() (Stats, error) {
	if f.statsErr != nil {
		return Stats{}, f.statsErr
	}
	return f.stats, nil
}

func (f fakeProbe) Filesystem(path string) (FSStats, error) {
	if f.fsErr != nil {
		return FSStats{}, f.fsErr
	}
	return f.fs[path], nil
}

func TestCheckDisabled(t *testing.T) {
	probe := fakeProbe{statsErr: errors.New("probe should not be called")}
	checker := NewCheckerWithProbe(Config{}, probe)
	if err := checker.Check(); err != nil {
		t.Fatalf("Check() with no thresholds = %v, want nil", err)
	}

	var nilChecker *Checker
	if err := nilChecker.Check(); err != nil {
		t.Fatalf("nil Checker.Check() = %v, want nil", err)
	}
}

func TestCheckThresholds(t *testing.T) {
	// 1000 total, 800 used = 80%.
	base := Stats{
		MemoryTotalBytes: 1000,
		MemoryUsedBytes:  800,
		SwapTotalBytes:   1000,
		SwapUsedBytes:    100,
		CPUPercent:       40,
	}

	tests := []struct {
		name    string
		cfg     Config
		probe   fakeProbe
		wantErr bool
		// wantSubstr, when set, must appear in the error message.
		wantSubstr string
	}{
		{
			name:  "all within limits",
			cfg:   Config{MemoryPercent: 90, SwapPercent: 50, CPUPercent: 90},
			probe: fakeProbe{stats: base},
		},
		{
			name:       "memory at limit fails",
			cfg:        Config{MemoryPercent: 80},
			probe:      fakeProbe{stats: base},
			wantErr:    true,
			wantSubstr: "memory usage",
		},
		{
			name:       "memory above limit fails",
			cfg:        Config{MemoryPercent: 50},
			probe:      fakeProbe{stats: base},
			wantErr:    true,
			wantSubstr: "memory usage",
		},
		{
			name:       "swap above limit fails",
			cfg:        Config{SwapPercent: 5},
			probe:      fakeProbe{stats: base},
			wantErr:    true,
			wantSubstr: "swap usage",
		},
		{
			name:       "cpu at limit fails",
			cfg:        Config{CPUPercent: 40},
			probe:      fakeProbe{stats: base},
			wantErr:    true,
			wantSubstr: "cpu usage",
		},
		{
			name: "no swap configured skips swap check",
			cfg:  Config{SwapPercent: 1},
			probe: fakeProbe{stats: Stats{
				MemoryTotalBytes: 1000, MemoryUsedBytes: 100,
				SwapTotalBytes: 0, SwapUsedBytes: 0,
			}},
		},
		{
			name:       "multiple violations reported together",
			cfg:        Config{MemoryPercent: 10, SwapPercent: 1, CPUPercent: 10},
			probe:      fakeProbe{stats: base},
			wantErr:    true,
			wantSubstr: "memory usage",
		},
		{
			name:       "probe error is propagated",
			cfg:        Config{MemoryPercent: 50},
			probe:      fakeProbe{statsErr: ErrUnsupported},
			wantErr:    true,
			wantSubstr: "not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewCheckerWithProbe(tt.cfg, tt.probe).Check()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Check() = nil, want error")
				}
				if tt.wantSubstr != "" && !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("Check() error = %q, want it to contain %q", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
		})
	}
}

func TestCheckFilesystems(t *testing.T) {
	probe := fakeProbe{fs: map[string]FSStats{
		"/":        {TotalBytes: 1000, UsedBytes: 950}, // 95%
		"/var":     {TotalBytes: 1000, UsedBytes: 500}, // 50%
		"/jails":   {TotalBytes: 1000, UsedBytes: 990}, // 99%
		"/missing": {TotalBytes: 0, UsedBytes: 0},      // 0%, no division by zero
	}}

	tests := []struct {
		name       string
		cfg        Config
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "all filesystems within limits",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/", MaxPercent: 96},
				{Path: "/var", MaxPercent: 80},
			}},
		},
		{
			name: "root over limit",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/", MaxPercent: 90},
				{Path: "/var", MaxPercent: 80},
			}},
			wantErr:    true,
			wantSubstr: "filesystem / usage",
		},
		{
			name: "second rule over limit",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/var", MaxPercent: 80},
				{Path: "/jails", MaxPercent: 95},
			}},
			wantErr:    true,
			wantSubstr: "filesystem /jails usage",
		},
		{
			name: "zero max percent disables rule",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/", MaxPercent: 0},
			}},
		},
		{
			name: "zero-total filesystem is not a violation",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/missing", MaxPercent: 1},
			}},
		},
		{
			name: "filesystem probe error is propagated",
			cfg: Config{Filesystems: []FilesystemRule{
				{Path: "/", MaxPercent: 90},
			}},
			wantErr:    true,
			wantSubstr: "filesystem",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := probe
			if tt.wantSubstr == "filesystem" {
				p = fakeProbe{fsErr: errors.New("statfs failed")}
			}
			err := NewCheckerWithProbe(tt.cfg, p).Check()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Check() = nil, want error")
				}
				if tt.wantSubstr != "" && !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("Check() error = %q, want it to contain %q", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
		})
	}
}

func TestParseFilesystemRules(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []FilesystemRule
		wantErr bool
	}{
		{name: "empty", in: "", want: nil},
		{name: "whitespace only", in: "   ", want: nil},
		{
			name: "single rule",
			in:   "/=90",
			want: []FilesystemRule{{Path: "/", MaxPercent: 90}},
		},
		{
			name: "multiple rules with whitespace",
			in:   " /=90 , /var=85 ,/usr/local/jails=95",
			want: []FilesystemRule{
				{Path: "/", MaxPercent: 90},
				{Path: "/var", MaxPercent: 85},
				{Path: "/usr/local/jails", MaxPercent: 95},
			},
		},
		{
			name: "fractional percent",
			in:   "/var=85.5",
			want: []FilesystemRule{{Path: "/var", MaxPercent: 85.5}},
		},
		{name: "missing equals", in: "/var", wantErr: true},
		{name: "empty path", in: "=90", wantErr: true},
		{name: "bad percent", in: "/var=abc", wantErr: true},
		{name: "percent too high", in: "/var=101", wantErr: true},
		{name: "negative percent", in: "/var=-1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFilesystemRules(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseFilesystemRules(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFilesystemRules(%q) unexpected error: %v", tt.in, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseFilesystemRules(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseFilesystemRules(%q)[%d] = %v, want %v", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "zero value", cfg: Config{}},
		{name: "valid", cfg: Config{MemoryPercent: 90, SwapPercent: 50, CPUPercent: 80}},
		{name: "memory too high", cfg: Config{MemoryPercent: 101}, wantErr: true},
		{name: "swap negative", cfg: Config{SwapPercent: -1}, wantErr: true},
		{name: "cpu too high", cfg: Config{CPUPercent: 100.1}, wantErr: true},
		{name: "filesystem missing path", cfg: Config{Filesystems: []FilesystemRule{{MaxPercent: 90}}}, wantErr: true},
		{name: "filesystem bad percent", cfg: Config{Filesystems: []FilesystemRule{{Path: "/", MaxPercent: 200}}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Fatal("zero Config should not be enabled")
	}
	for _, cfg := range []Config{
		{MemoryPercent: 1},
		{SwapPercent: 1},
		{CPUPercent: 1},
		{Filesystems: []FilesystemRule{{Path: "/", MaxPercent: 1}}},
	} {
		if !cfg.Enabled() {
			t.Fatalf("Config %+v should be enabled", cfg)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
