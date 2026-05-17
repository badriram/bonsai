package aws

// Phase 1: single EC2 in public subnet, runs the k3s server with embedded
// SQLite (default). Phase 3 replaces this with a 3-node ASG + embedded etcd
// behind an NLB.
//
// User data: /assets/userdata/server.sh — installs k3s in server mode,
// writes the cluster token to SSM at /bonsai/<name>/<env>/token.
