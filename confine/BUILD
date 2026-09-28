# Package confine, and the tests that start sandboxes. Those cannot run
# inside a sandbox: the label lets a confined runner leave them out with
# `--exclude sandbox`. The test main is generated from every source given,
# whatever its build constraint, so the sources are chosen here.

if CONFIG.OS == "darwin":
    backend_srcs = ["backend_darwin.go"]
    escape_srcs = [
        "escape_darwin_test.go",
        "suite_sandbox_test.go",
    ]
elif CONFIG.OS == "linux":
    backend_srcs = ["backend_linux.go"]
    escape_srcs = [
        "escape_linux_test.go",
        "suite_sandbox_test.go",
    ]
else:
    backend_srcs = ["backend_other.go"]
    escape_srcs = ["suite_other_test.go"]

go_library(
    name = "confine",
    srcs = [
        "backend.go",
        "bwrap.go",
        "confine.go",
        "verify.go",
    ] + backend_srcs,
    visibility = ["PUBLIC"],
    deps = ["//:mcpkit"],
)

# The escape suite, written against the public API for every backend.
go_test(
    name = "escape_test",
    srcs = ["escape_test.go"] + escape_srcs,
    external = True,
    labels = ["sandbox"],
    deps = [
        ":confine",
        "//:mcpkit",
    ],
)

# What the public API cannot reach: a missing or refused sandbox, the
# rendering. bwrap_test.go builds on every system, so the rendering of
# bubblewrap is tested everywhere.
go_test(
    name = "backend_test",
    srcs = ["bwrap_test.go"] + (["backend_darwin_test.go"] if CONFIG.OS == "darwin" else []),
    labels = ["sandbox"],
    deps = [
        ":confine",
        "//:mcpkit",
    ],
)

# Probes of what Seatbelt can express, through /usr/bin/sandbox-exec
# (docs/confinement-macos.md).
go_test(
    name = "confine_test",
    srcs = ["doc_test.go"] + (["probe_darwin_test.go"] if CONFIG.OS == "darwin" else []),
    labels = ["sandbox"],
)

# Probes of what bubblewrap can do (docs/confinement-linux.md).
go_test(
    name = "bwrap_probe_test",
    srcs = ["bwrap_probe_linux_test.go" if CONFIG.OS == "linux" else "bwrap_probe_other_test.go"],
    labels = ["sandbox"],
)

# The sources //:mcpkit_test parses to check its imports.
filegroup(
    name = "sources",
    srcs = glob(["*.go"]),
    visibility = ["PUBLIC"],
)

lint()
