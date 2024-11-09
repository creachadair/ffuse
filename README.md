# ffuse: FUSE driver for FFS

[![GoDoc](https://img.shields.io/static/v1?label=godoc&message=reference&color=yellowgreen)](https://pkg.go.dev/github.com/creachadair/ffuse)
[![CI](https://github.com/creachadair/ffuse/actions/workflows/go-presubmit.yml/badge.svg?event=push&branch=main)](https://github.com/creachadair/ffuse/actions/workflows/go-presubmit.yml)

This module defines a FUSE filesystem that exposes the [FFS](https://github.com/creachadair/ffs) data format.

**Note:** The `ffuse` tool that used to live here has been susperseded by the
`mount` subcommand of the [`ffs` tool](https://github.com/creachadair/ffstools/blob/main/ffs).
