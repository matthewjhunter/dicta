module github.com/matthewjhunter/dicta

go 1.25.0

require github.com/matthewjhunter/asrclient v0.0.0-00010101000000-000000000000

require go.uber.org/goleak v1.3.0 // indirect

require (
	gioui.org v0.9.0
	gioui.org/shader v1.0.8 // indirect
	github.com/go-text/typesetting v0.3.0 // indirect
	golang.org/x/exp/shiny v0.0.0-20250408133849-7e4ce0ab07d0 // indirect
	golang.org/x/image v0.26.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.24.0 // indirect
)

replace github.com/matthewjhunter/asrclient => ../../go/src/github.com/matthewjhunter/asrclient
