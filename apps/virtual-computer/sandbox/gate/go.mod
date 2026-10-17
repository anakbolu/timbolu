// The sandbox's gate is its own module for the same reason the control plane is
// (docs/adr/0003-virtual-computer.md §3.2): it is one small static binary with no
// dependency graph of its own, built in a stage of the sandbox image and copied into
// it. It imports nothing but the standard library, so there is no go.sum here and no
// `replace` pointing at the sibling copy of packages/.
//
// It is separate from the control plane rather than a package inside it because the
// two run in different trust domains: the gate runs inside the sandbox, which is the
// one place in this system that runs code a model asked for, and the control plane
// runs on the host with the docker socket. Neither may import the other.
module github.com/robzlabz/employeebot/apps/virtual-computer/sandbox/gate

go 1.26.0
