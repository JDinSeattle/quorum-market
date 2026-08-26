variable "name" {
  description = "Cluster name, used in resource names and tags."
  type        = string
}

variable "project_name" { type = string }
variable "aws_region" { type = string }
variable "image" { type = string }
variable "sg_id" { type = string }
variable "key_name" { type = string }
variable "instance_profile_name" { type = string }
variable "instance_type" { type = string }

variable "subnet_id" {
  description = <<-EOT
    Subnet holding every node.

    The whole cluster sits in one subnet so each node can be given a static
    private IP carved from that subnet's CIDR. Nodes have to know each other's
    addresses at boot, and a static assignment is the only way to know them
    before any instance exists. The cost is that the cluster shares an
    availability zone; for a load-testing stack that is an acceptable trade,
    and it is called out here so it is a decision rather than an accident.
  EOT
  type        = string
}

variable "node_count" {
  description = "Number of replicas."
  type        = number
}

variable "ip_offset" {
  description = "First host number to assign inside the subnet CIDR. Each cluster needs its own range."
  type        = number
}

variable "mode" {
  description = "Replication strategy: leader-follower or leaderless."
  type        = string

  validation {
    condition     = contains(["leader-follower", "leaderless"], var.mode)
    error_message = "mode must be leader-follower or leaderless."
  }
}

variable "write_quorum" { type = number }
variable "read_quorum" { type = number }

variable "write_delay_ms" {
  description = "Simulated cost of making a write durable on one node."
  type        = number
  default     = 5
}

variable "read_delay_ms" {
  description = "Simulated cost of a storage read on one node."
  type        = number
  default     = 2
}
