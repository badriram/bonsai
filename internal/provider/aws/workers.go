package aws

// Workers run as an ASG. New instances boot, pull the cluster token from SSM,
// and join via the standard k3s agent install. Scaling in/out is a single
// SetDesiredCapacity call.
//
// Rotation (for AMI/k3s updates) is an ASG instance refresh — termination is
// gated by a lifecycle hook that drains the k8s node before the EC2 dies.
//
// User data: /assets/userdata/worker.sh
