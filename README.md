# ffuse: FUSE driver for FFS

**Note:** The `cmd/ffuse` tool has been susperseded by the `mount` subcommand of the [`ffs` tool](https://github.com/creachadair/ffs/blob/main/ffs) and will probably be removed eventually.

[![GoDoc](https://img.shields.io/static/v1?label=godoc&message=reference&color=yellowgreen)](https://pkg.go.dev/github.com/creachadair/ffuse)

This module defines a FUSE filesystem that exposes the FFS data format.

See also https://github.com/creachadair/ffs.

## Installation

```sh
go install github.com/creachadair/ffuse/cmd/ffuse@latest
```
