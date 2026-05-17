package aws

// VPC layout (intentional): public subnets only, Internet Gateway, no NAT.
//
// Rationale: every dependency the cluster talks to (Neon, Cognito, Bedrock,
// OpenRouter, S3) lives outside AWS or on a public endpoint. A NAT Gateway
// would add ~$32-98/month of fixed cost to route traffic that doesn't need it.
//
// Security posture is enforced via security groups, not subnet privacy:
//   - 6443 (k3s API):       admin CIDR + worker SG
//   - 10250 (kubelet):      intra-cluster SG only
//   - 22 (SSH):             admin CIDR only (optional — disabled by default)
//   - egress:               open
//
// All resources tagged: bonsai:cluster=<name>, bonsai:env=<env>, bonsai:managed=true
