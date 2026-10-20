package paths

import "testing"

func TestSyncedFolderRisk(t *testing.T) {
	for root, want := range map[string]bool{
		`C:\Users\张三\OneDrive\文档\Kanade`:                               true,
		`C:\Users\a\OneDrive - Contoso\Documents\Kanade`:               true,
		"/Users/a/Library/Mobile Documents/com~apple~CloudDocs/Kanade": true,
		`C:\Users\a\Documents\Kanade`:                                  false,
		"/Users/a/Documents/Kanade":                                    false,
		"/Users/a/Documents/MyOneDriveNotes/Kanade":                    false,
	} {
		if got := SyncedFolderRisk(root); got != want {
			t.Errorf("SyncedFolderRisk(%q) = %v, want %v", root, got, want)
		}
	}
}
