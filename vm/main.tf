# The test cluster's VMs (doc/testing.md): k3s on two Incus virtual machines, the server
# retycfs-csi and, with CLUSTER_MODE=multi, the agent retycfs-csi-agent, on their own network.
#
# From this directory, next to its settings (.env, .env.local):
#   tofu init       # once: fetches the Incus provider
#   tofu apply      # creates what is missing, and waits for the provisioning
#   tofu destroy    # deletes the VMs and the network
#   tofu apply -var running=false   # shuts them down (make vm-stop); tofu apply starts them again
#
# Each VM is provisioned by cloud-init at its first boot (cloud-init-server.yaml,
# cloud-init-agent.yaml); apply returns once it is done, and fails if it fails. Both install k3s'
# stable release at their creation; an agent may not be newer than its server, so recreate both
# together (tofu apply -replace=incus_instance.server ...).

terraform {
  required_version = ">= 1.6"
  required_providers {
    incus = {
      source  = "lxc/incus"
      version = "~> 1.2"
    }
  }
}

provider "incus" {}

variable "running" {
  description = "false shuts the VMs down, keeping them (make vm-stop)."
  type        = bool
  default     = true
}

# The cluster's settings: .env, then .env.local over it, as the Makefile reads them (vm/.env). Only
# CLUSTER_MODE matters here.
locals {
  env_lines = flatten([
    for f in [".env", ".env.local"] :
    fileexists("${path.module}/${f}") ? split("\n", file("${path.module}/${f}")) : []
  ])
  env = {
    for kv in [for l in local.env_lines : regex("^([A-Za-z_][A-Za-z0-9_]*)=(.*)$", trimspace(l)) if can(regex("^[A-Za-z_][A-Za-z0-9_]*=", trimspace(l)))] :
    kv[0] => trimspace(kv[1])...
  }
  cluster_mode = try(element(local.env["CLUSTER_MODE"], length(local.env["CLUSTER_MODE"]) - 1), "")

  image     = "images:debian/13/cloud"
  host_ip   = "192.168.57.1"
  server_ip = "192.168.57.10"
  agent_ip  = "192.168.57.11"
  # A fixed join token, for local test VMs only.
  k3s_token = "retycfs-csi-vm"
  cloud_init_vars = {
    server_ip = local.server_ip
    agent_ip  = local.agent_ip
    k3s_token = local.k3s_token
  }

  # Waits for cloud-init at the creation of a VM; on a failure, shows the end of its log.
  wait_for_provisioning = {
    provisioning = {
      command = ["sh", "-c", "cloud-init status --wait >/dev/null || { tail -n 40 /var/log/cloud-init-output.log >&2; exit 1; }"]
      trigger = "once"
      timeout = "20m"
    }
  }
}

# Their network; the host, at 192.168.57.1, is reachable from the VMs and their pods (the dev API,
# examples/helm-values/dev-vm-values.yaml).
resource "incus_network" "retycfs" {
  name = "retycfs"
  config = {
    "ipv4.address" = "${local.host_ip}/24"
    "ipv4.nat"     = "true"
    "ipv6.address" = "none"
  }
}

resource "incus_instance" "server" {
  name    = "retycfs-csi"
  type    = "virtual-machine"
  image   = local.image
  running = var.running

  config = {
    "limits.cpu"           = "2"
    "limits.memory"        = "4GiB"
    "cloud-init.user-data" = templatefile("${path.module}/cloud-init-server.yaml", local.cloud_init_vars)
  }

  device {
    name = "eth0"
    type = "nic"
    properties = {
      network        = incus_network.retycfs.name
      "ipv4.address" = local.server_ip
    }
  }

  device {
    name = "root"
    type = "disk"
    properties = {
      pool = "default"
      path = "/"
      size = "20GiB"
    }
  }

  wait_for {
    type = "agent"
  }
  exec = var.running ? local.wait_for_provisioning : {}

  lifecycle {
    precondition {
      condition     = contains(["multi", "single"], local.cluster_mode)
      error_message = "CLUSTER_MODE must be multi or single (.env, .env.local), not \"${local.cluster_mode}\"."
    }
  }
}

# Second node, with CLUSTER_MODE=multi. Created along with the server: its cloud-init waits for it.
resource "incus_instance" "agent" {
  count = local.cluster_mode == "multi" ? 1 : 0

  name    = "retycfs-csi-agent"
  type    = "virtual-machine"
  image   = local.image
  running = var.running

  config = {
    "limits.cpu"           = "2"
    "limits.memory"        = "3GiB"
    "cloud-init.user-data" = templatefile("${path.module}/cloud-init-agent.yaml", local.cloud_init_vars)
  }

  device {
    name = "eth0"
    type = "nic"
    properties = {
      network        = incus_network.retycfs.name
      "ipv4.address" = local.agent_ip
    }
  }

  device {
    name = "root"
    type = "disk"
    properties = {
      pool = "default"
      path = "/"
      size = "20GiB"
    }
  }

  wait_for {
    type = "agent"
  }
  exec = var.running ? local.wait_for_provisioning : {}
}

output "urls" {
  description = "From the host"
  value = {
    privatebin = "http://${local.server_ip}"
    jaeger     = "http://${local.server_ip}:8081"
    prometheus = "http://${local.server_ip}:8082"
  }
}
