output "node_urls" {
  description = "Internal URL of every node."
  value       = [for ip in local.ips : "http://${ip}:8080"]
}

output "entrypoint" {
  description = <<-EOT
    The URL clients should use.

    Under leader-follower this is the leader, because it is the only node that
    accepts writes. Under leaderless any node would do; node 0 is picked so the
    address is stable.
  EOT
  value       = "http://${local.ips[0]}:8080"
}

output "private_ips" {
  value = local.ips
}
