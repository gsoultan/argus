//go:build tools

package mobile

// gomobile bind generates code that imports golang.org/x/mobile/bind, so
// the module must require it; this keeps `go mod tidy` from dropping it.
// Never built: the tools tag is not set anywhere.
import _ "golang.org/x/mobile/bind"
