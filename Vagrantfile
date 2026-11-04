# -*- mode: ruby -*-
# vi: set ft=ruby :
#
# k3s test VMs for retycfs-k8s-csi (Debian 13 "trixie", libvirt/KVM). See doc/testing.md.
#
# `server`, the k3s server where the Makefile's vm-* targets run kubectl and helm, and with
# CLUSTER_MODE=multi (.env, .env.local) `agent`, which joins it at its provisioning. `vagrant up`
# brings them up; `vagrant halt`, `vagrant destroy -f` and `vagrant ssh server` do the rest.
#
# Host requirements: vagrant + vagrant-libvirt (`vagrant plugin install vagrant-libvirt`), a
# running libvirtd with the qemu:///system URI, and your user in the `libvirt` group.

# A fixed address each on a private network, where k3s and flannel run, so the agent knows where
# the server is; a fixed join token, for local test VMs only. Both install k3s' stable release at
# the same `vagrant up`; an agent may not be newer than its server, so recreate both together.
# .env, then .env.local over it: KEY=value lines, as the Makefile reads them too.
CLUSTER = [".env", ".env.local"].each_with_object({}) do |name, settings|
  path = File.join(__dir__, name)
  next unless File.exist?(path)
  File.foreach(path) do |line|
    key, value = line.strip.split("=", 2)
    settings[key] = value.to_s.strip unless key.nil? || key.empty? || key.start_with?("#")
  end
end
unless %w[multi single].include?(CLUSTER["CLUSTER_MODE"])
  abort "CLUSTER_MODE must be multi or single (.env, .env.local), not #{CLUSTER["CLUSTER_MODE"].inspect}"
end

K3S_TOKEN = "retycfs-csi-vagrant"
SERVER_IP = "192.168.56.10"
AGENT_IP = "192.168.56.11"

Vagrant.configure("2") do |config|
  config.vm.box = "debian/trixie64"

  config.vm.provision "shell", name: "base", inline: <<~'SHELL'
    set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    apt-get install -y -q --no-install-recommends fuse3 curl ca-certificates rsync jq vim htop

    modprobe fuse
    echo fuse > /etc/modules-load.d/fuse.conf
    test -c /dev/fuse
  SHELL

  # k3s pinned to the private network: --node-ip, and flannel on the interface holding it (the
  # first interface is libvirt's NAT network, the one vagrant ssh uses).
  k3s_env = { "K3S_TOKEN" => K3S_TOKEN, "SERVER_IP" => SERVER_IP }

  config.vm.define "server", primary: true do |server|
    server.vm.hostname = "retycfs-csi"
    server.vm.network :private_network, ip: SERVER_IP

    server.vm.provider :libvirt do |lv|
      lv.driver = "kvm"
      lv.cpus = 2
      lv.memory = 4096
    end

    # Traefik's entrypoints: web (the examples' ingresses), jaeger (the Jaeger UI) and prometheus
    # (the Prometheus UI).
    server.vm.network "forwarded_port", guest: 80, host: 18080
    server.vm.network "forwarded_port", guest: 8081, host: 18081
    server.vm.network "forwarded_port", guest: 8082, host: 18082

    server.vm.synced_folder ".", "/vagrant", type: "rsync",
      rsync__exclude: [".git/", ".vagrant/", "retycfs-k8s-csi", "dist/", ".env.local", ".env.dev"]

    server.vm.provision "shell", name: "k3s", env: k3s_env, inline: <<~'SHELL'
      set -euo pipefail

      # k3s, with its bundled Traefik serving the examples' ingresses; a 0644 kubeconfig lets the
      # vagrant user run kubectl without sudo (KUBECONFIG is exported for login shells, which is what
      # `vagrant ssh -c` uses).
      if ! command -v k3s >/dev/null 2>&1; then
        iface=$(ip -o -4 addr show | awk -v ip="$SERVER_IP" '$4 ~ "^" ip "/" { print $2 }')
        curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --write-kubeconfig-mode 644 --node-ip $SERVER_IP --flannel-iface $iface" sh -
      fi
      echo 'export KUBECONFIG=/etc/rancher/k3s/k3s.yaml' > /etc/profile.d/k3s.sh

      until k3s kubectl wait --for=condition=Ready "node/$(hostname)" --timeout=10s >/dev/null 2>&1; do sleep 2; done
      k3s kubectl get nodes

      # Helm, the way the driver is deployed (make vm-helm). Debian has no package; the official
      # installer verifies the release checksum before installing to /usr/local/bin.
      if ! command -v helm >/dev/null 2>&1; then
        curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
      fi
      helm version --short
    SHELL

    # Jaeger all-in-one (in-memory storage, the chart's default) collecting the retyc CLI's traces
    # (OTEL_* in examples/helm-values/*-vagrant-values.yaml), its UI on http://localhost:18081. Traefik gets an
    # entrypoint for it, and one for Prometheus (provisioned below); web is made the default one, so
    # host-less ingresses such as the examples' stay on port 80 instead of also catching these ports.
    server.vm.provision "shell", name: "jaeger", inline: <<~'SHELL'
      set -euo pipefail
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

      cat > /var/lib/rancher/k3s/server/manifests/traefik-config.yaml <<'EOF'
      apiVersion: helm.cattle.io/v1
      kind: HelmChartConfig
      metadata:
        name: traefik
        namespace: kube-system
      spec:
        valuesContent: |-
          ports:
            web:
              asDefault: true
            jaeger:
              port: 8081
              expose:
                default: true
              exposedPort: 8081
              protocol: TCP
            prometheus:
              port: 8082
              expose:
                default: true
              exposedPort: 8082
              protocol: TCP
      EOF

      helm repo add jaegertracing https://jaegertracing.github.io/helm-charts >/dev/null
      helm repo update jaegertracing >/dev/null
      helm upgrade --install jaeger jaegertracing/jaeger --version 4.14.1 \
        -n jaeger --create-namespace --wait --timeout 5m -f - <<'EOF'
      jaeger:
        ingress:
          enabled: true
          annotations:
            traefik.ingress.kubernetes.io/router.entrypoints: jaeger
          hosts: [""]
      EOF
    SHELL

    # Prometheus, through the Prometheus Operator, scraping the driver and the mount pods through the
    # chart's PodMonitors (metrics.podMonitor in examples/helm-values/*-vagrant-values.yaml), its UI on
    # http://localhost:18082. Only the operator and Prometheus, scraping retycfs only: no Grafana, no
    # Alertmanager, no exporters, no Kubernetes components, no self-monitoring. Any PodMonitor and
    # ServiceMonitor of the cluster is picked, with no label required. Before `make vm-helm`, which
    # needs the CRDs.
    server.vm.provision "shell", name: "monitoring", inline: <<~'SHELL'
      set -euo pipefail
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

      helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
      helm repo update prometheus-community >/dev/null
      helm upgrade --install monitoring prometheus-community/kube-prometheus-stack --version 92.17.1 \
        -n monitoring --create-namespace --wait --timeout 10m -f - <<'EOF'
      alertmanager: { enabled: false }
      grafana: { enabled: false }
      nodeExporter: { enabled: false }
      kubeStateMetrics: { enabled: false }
      defaultRules: { create: false }
      kubeControllerManager: { enabled: false }
      kubeScheduler: { enabled: false }
      kubeEtcd: { enabled: false }
      kubeProxy: { enabled: false }
      kubeApiServer: { enabled: false }
      kubelet: { enabled: false }
      coreDns: { enabled: false }
      prometheusOperator:
        serviceMonitor: { selfMonitor: false }
      prometheus:
        serviceMonitor: { selfMonitor: false }
        ingress:
          enabled: true
          annotations:
            traefik.ingress.kubernetes.io/router.entrypoints: prometheus
          hosts: []
        prometheusSpec:
          podMonitorSelectorNilUsesHelmValues: false
          serviceMonitorSelectorNilUsesHelmValues: false
          scrapeInterval: 15s
          retention: 2d
          resources:
            requests: { memory: 256Mi }
            limits: { memory: 768Mi }
      EOF
    SHELL
  end

  # Second node, with CLUSTER_MODE=multi
  config.vm.define "agent", autostart: CLUSTER["CLUSTER_MODE"] == "multi" do |agent|
    agent.vm.hostname = "retycfs-csi-agent"
    agent.vm.network :private_network, ip: AGENT_IP
    agent.vm.synced_folder ".", "/vagrant", disabled: true

    agent.vm.provider :libvirt do |lv|
      lv.driver = "kvm"
      lv.cpus = 2
      lv.memory = 3072
    end

    agent.vm.provision "shell", name: "k3s-agent", env: k3s_env.merge("AGENT_IP" => AGENT_IP), inline: <<~'SHELL'
      set -euo pipefail
      if ! command -v k3s >/dev/null 2>&1; then
        until curl -skf "https://$SERVER_IP:6443/cacerts" >/dev/null; do sleep 5; done
        iface=$(ip -o -4 addr show | awk -v ip="$AGENT_IP" '$4 ~ "^" ip "/" { print $2 }')
        curl -sfL https://get.k3s.io | K3S_URL="https://$SERVER_IP:6443" \
          INSTALL_K3S_EXEC="agent --node-ip $AGENT_IP --flannel-iface $iface" sh -
      fi
      systemctl is-active k3s-agent
    SHELL
  end
end
