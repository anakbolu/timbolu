// The control plane is its own module for one reason: the PoC must not be able to
// change the backend's build. It has no Temporal, no Postgres, and no product code,
// and it depends on nothing the backend depends on (docs/adr/0003-virtual-computer.md
// §3.2). Its only dependency is the standard library, which is also why the image it
// ships in stays small.
module github.com/anakbolu/timbolu/apps/virtual-computer/control

go 1.26.0
