package ignore

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		name, rule, file string
		want             bool
	}{
		{"suffix root", "*.log", "app.log", true}, {"suffix nested", "*.log", "a/b/app.log", true},
		{"suffix mismatch", "*.log", "app.log.go", false}, {"directory name", "logs/", "a/logs/app.txt", true},
		{"root directory", "/logs/", "a/logs/app.txt", false}, {"recursive zero segments", "src/**/*.log", "src/app.log", true},
		{"recursive many segments", "src/**/*.log", "src/a/b/app.log", true}, {"root relative", "src/**/*.log", "other/src/app.log", false},
		{"directory descendant", "logs/**", "logs/deep/file.txt", true}, {"question mark", "file?.txt", "a/file1.txt", true},
		{"normalize", "*.log", "src/../app.log", true}, {"unicode", "*.日志", "目录/运行.日志", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate([]string{tt.rule}); err != nil {
				t.Fatal(err)
			}
			if got := Match([]string{tt.rule}, tt.file); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, rule := range []string{"", "!*.log", "../logs", "a//b", "a/**b", "[abc]", "{a,b}", "a\\b", "/", "a\x00b", "./logs"} {
		t.Run(rule, func(t *testing.T) {
			if Validate([]string{rule}) == nil {
				t.Fatal("expected error")
			}
		})
	}
}
