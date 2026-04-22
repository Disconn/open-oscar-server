package state

import "testing"

func TestICQ6GenerationClient(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"AOL Instant Messenger", false},
		{"ICQ 2000b", false},
		{"ICQ 2001A", false},
		{"ICQ 2002a", true},
		{"ICQ 2002 Pro", true},
		{"ICQ 2003a", true},
		{"ICQ 2003b build 3816", true},
		{"Mirabilis ICQ 6", true},
		{"icq6 lite", true},
		{"Mirabilis ICQ build 6.5", true},
		{"ICQ for Windows 6.5", true},
		{"ICQ 6.0", true},
		{"ICQ Client 7.0", true},
		{"localized ICQ 8", true},
		{"ICQ Inc.", false},
		{"ICQ Client", false},
		{"icq lite 5", false},
		{"Mirabilis ICQ 5", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := ICQ6GenerationClient(tt.id); got != tt.want {
				t.Fatalf("ICQ6GenerationClient(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
