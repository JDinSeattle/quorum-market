# A cluster of KV nodes. One module covers both replication strategies because
# one binary implements both; only the environment differs.

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }
}

data "aws_subnet" "target" {
  id = var.subnet_id
}

locals {
  # Static addresses so every node knows its peers before any of them boot.
  ips = [for i in range(var.node_count) : cidrhost(data.aws_subnet.target.cidr_block, var.ip_offset + i)]

  # Under leaderless replication every node lists all the others. Under
  # leader-follower only the leader holds a peer list; followers never
  # coordinate, so they need none.
  peers = [
    for i in range(var.node_count) :
    join(",", [for j in range(var.node_count) : "http://${local.ips[j]}:8080" if j != i])
  ]

  followers = join(",", [for j in range(1, var.node_count) : "http://${local.ips[j]}:8080"])
}

resource "aws_instance" "node" {
  count = var.node_count

  ami                         = data.aws_ami.al2023.id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  private_ip                  = local.ips[count.index]
  vpc_security_group_ids      = [var.sg_id]
  iam_instance_profile        = var.instance_profile_name
  key_name                    = var.key_name
  associate_public_ip_address = true
  user_data_replace_on_change = true

  tags = {
    Name    = "${var.project_name}-${var.name}-${count.index}"
    Cluster = var.name
    Role    = var.mode == "leader-follower" ? (count.index == 0 ? "leader" : "follower") : "peer"
  }

  user_data = <<-EOF
    #!/bin/bash
    set -euxo pipefail

    dnf install -y docker
    systemctl enable --now docker

    aws ecr get-login-password --region ${var.aws_region} \
      | docker login --username AWS --password-stdin ${split("/", var.image)[0]}

    docker run -d --name kv-node --restart always \
      -p 8080:8080 \
      -e SERVER_PORT=8080 \
      -e NODE_ID=${var.name}-${count.index} \
      -e NODE_MODE=${var.mode} \
      -e ROLE=${count.index == 0 ? "leader" : "follower"} \
      -e FOLLOWER_URLS=${var.mode == "leader-follower" && count.index == 0 ? local.followers : ""} \
      -e PEER_URLS=${var.mode == "leaderless" ? local.peers[count.index] : ""} \
      -e WRITE_QUORUM_SIZE=${var.write_quorum} \
      -e READ_QUORUM_SIZE=${var.read_quorum} \
      -e WRITE_DELAY_MS=${var.write_delay_ms} \
      -e READ_DELAY_MS=${var.read_delay_ms} \
      ${var.image} /usr/local/bin/kvnode
  EOF
}
