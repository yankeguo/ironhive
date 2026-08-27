package main

import "testing"

func TestMatchAsset(t *testing.T) {
	files := []string{".gitkeep", "home-1a2b3c4d.js", "main-5e6f7a8b.css", "about.js"}
	tests := []struct {
		name, ext, want string
	}{
		{"home", "js", "home-1a2b3c4d.js"},
		{"main", "css", "main-5e6f7a8b.css"},
		{"about", "js", "about.js"},
		{"missing", "js", ""},
		{"home", "css", ""},
	}
	for _, tt := range tests {
		if got := matchAsset(files, tt.name, tt.ext); got != tt.want {
			t.Errorf("matchAsset(%q, %q) = %q, want %q", tt.name, tt.ext, got, tt.want)
		}
	}
}
