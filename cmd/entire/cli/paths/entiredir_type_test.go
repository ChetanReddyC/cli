package paths

import (
	"io/fs"
	"testing"
)

// The entries directly under `.entire` are Entire's own, and Entire only ever
// creates regular files and directories there. Anything else arrived some other
// way, so the check is an allowlist rather than a list of known-bad types: a
// mode bit nobody has thought about yet is refused by default.
//
// ModeIrregular is the deliberate exception, and the reason it cannot be part
// of the allowlist is that Windows overloads it. Go maps every reparse tag it
// has no category for onto that single bit, which puts NTFS directory junctions
// and OneDrive Files On-Demand placeholders in the same bucket. Refusing the
// bucket would hard-fail every command in a repo inside a synced folder, and
// the placeholder gets there without anyone attacking anything, while a
// junction cannot arrive by checkout at all: git has no tree-object mode for
// one. Tolerating the ambiguous bit is the cheaper mistake.
func TestUnsupportedEntryType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode fs.FileMode
		want bool
	}{
		{name: "regular file", mode: 0},
		{name: "directory", mode: fs.ModeDir},
		{name: "symbolic link", mode: fs.ModeSymlink, want: true},
		{name: "named pipe", mode: fs.ModeNamedPipe, want: true},
		{name: "socket", mode: fs.ModeSocket, want: true},
		{name: "block device", mode: fs.ModeDevice, want: true},
		{name: "character device", mode: fs.ModeDevice | fs.ModeCharDevice, want: true},
		{name: "irregular is tolerated", mode: fs.ModeIrregular},
		{
			// Nothing produces this today, and if something starts to, the
			// symlink half is the half that matters.
			name: "irregular alongside a rejected bit is still rejected",
			mode: fs.ModeIrregular | fs.ModeSymlink,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := unsupportedEntryType(tt.mode); got != tt.want {
				t.Errorf("unsupportedEntryType(%v) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
