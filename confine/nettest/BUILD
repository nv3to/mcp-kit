# The network cases of the escape suite: a confined command against a real
# egress proxy. They live here so that neither //confine nor //egress depends
# on the other. The test main is generated from every source given, whatever
# its build constraint, so the sources are chosen here. The cases of the
# socket through which a command on Linux reaches the proxy skip elsewhere.

if CONFIG.OS == "darwin":
    suite_srcs = [
        "socket_other_test.go",
        "suite_sandbox_test.go",
    ]
elif CONFIG.OS == "linux":
    suite_srcs = [
        "socket_linux_test.go",
        "suite_sandbox_test.go",
    ]
else:
    suite_srcs = [
        "socket_other_test.go",
        "suite_other_test.go",
    ]

go_test(
    name = "nettest",
    srcs = ["network_test.go"] + suite_srcs,
    external = True,
    labels = ["sandbox"],
    deps = [
        "//confine",
        "//egress",
    ],
)

lint()
