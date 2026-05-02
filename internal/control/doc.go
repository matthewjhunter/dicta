// Package control implements the Unix socket server at
// $XDG_RUNTIME_DIR/dicta.sock (mode 0600).
//
// Wire format is newline-delimited JSON with a 64 KiB max line. Connections
// start in command mode (one request, one response). After a {"cmd":
// "subscribe"} the connection locks into event mode and the daemon pushes
// JSON events; further commands are rejected. See §5.6 of the design doc.
package control
