# Embedded helper binaries

`make helpers` writes `shunt-helper-linux-amd64` and `shunt-helper-linux-arm64`
here, and `go:embed` bakes them into the `shunt` CLI so it ships as a single
file with no Go toolchain required on the operator's machine.

This README exists so the `go:embed bin` directive still resolves in a fresh
checkout where the binaries have not been built yet — in that case the CLI falls
back to compiling the helper on demand.
