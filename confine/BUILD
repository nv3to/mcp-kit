# Probes of what a Seatbelt profile can express. They start sandboxes through
# /usr/bin/sandbox-exec, so they cannot run inside one: the label lets a
# confined runner leave them out with `--exclude sandbox`.
go_test(
    name = "confine_test",
    srcs = glob(["*_test.go"]),
    labels = ["sandbox"],
)

lint()
