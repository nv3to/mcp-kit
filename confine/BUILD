# Probes of what each sandbox can express. They start sandboxes, so they
# cannot run inside one: the label lets a confined runner leave them out
# with `--exclude sandbox`. The test main is generated from every source
# given, whatever its build constraint, so the sources are chosen here.

# Seatbelt, through /usr/bin/sandbox-exec (docs/confinement-macos.md).
go_test(
    name = "confine_test",
    srcs = ["doc_test.go"] + (["probe_darwin_test.go"] if CONFIG.OS == "darwin" else []),
    labels = ["sandbox"],
)

# bubblewrap (docs/confinement-linux.md).
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
