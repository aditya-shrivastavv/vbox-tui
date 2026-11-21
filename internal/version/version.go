// Package version holds build information for vbox-tui.
package version

// Version is the application version. It can be overridden at build time with:
//
//	go build -ldflags "-X github.com/aditya-shrivastavv/vbox-tui/internal/version.Version=1.2.3"
var Version = "0.1.0"

// Name is the human readable application name.
const Name = "vbox-tui"

// UserAgent is used in a couple of status messages.
func String() string {
	return Name + " " + Version
}
