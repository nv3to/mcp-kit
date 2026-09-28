# The network cases of the escape suite: a confined command against a real
# egress proxy. They live here so that neither //confine nor //egress depends
# on the other. The test main is generated from every source given, whatever
# its build constraint, so the sources are chosen here.
go_test(
    name = "nettest",
    srcs = ["network_test.go"] +
           (["suite_darwin_test.go"] if CONFIG.OS == "darwin" else ["suite_other_test.go"]),
    external = True,
    labels = ["sandbox"],
    deps = [
        "//confine",
        "//egress",
    ],
)

lint()
