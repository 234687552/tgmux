package sanitize

import "regexp"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`key-[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`Bearer [a-zA-Z0-9\-._~+/]+=*`),
	regexp.MustCompile(`(?i)token[=:]\s*["']?[a-zA-Z0-9\-._]{20,}`),
	regexp.MustCompile(`(?i)password[=:]\s*["']?[^\s"']{8,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]* PRIVATE KEY-----`),
}

func Redact(text string, enabled bool) string {
	if !enabled {
		return text
	}
	for _, p := range patterns {
		text = p.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
