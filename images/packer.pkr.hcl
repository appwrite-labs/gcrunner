packer {
  required_plugins {
    googlecompute = {
      version = ">= 1.1.0"
      source  = "github.com/hashicorp/googlecompute"
    }
  }
}

locals {
  timestamp  = formatdate("YYYYMMDDhhmmss", timestamp())
  # GCE names and label values allow only lowercase letters, digits, - and _ (63 chars).
  git_sha    = lower(regex_replace(var.git_sha, "[^a-zA-Z0-9_-]", "-"))
  git_ref    = substr(lower(regex_replace(var.git_ref, "[^a-zA-Z0-9_-]", "-")), 0, 63)
  # Name images after the commit they were built from; the timestamp keeps rebuilds unique.
  image_name = local.git_sha != "" ? "${var.image_family}-${local.git_sha}-${local.timestamp}" : "${var.image_family}-${local.timestamp}"
}

source "googlecompute" "runner" {
  project_id              = var.project_id
  zone                    = var.zone
  source_image_family     = var.base_image_family
  source_image_project_id = [var.base_image_project]
  image_name              = local.image_name
  image_family            = var.image_family
  image_description       = var.image_description
  image_labels = {
    git-sha = local.git_sha
    git-ref = local.git_ref
  }
  machine_type            = var.machine_type
  disk_size               = var.disk_size
  access_token            = var.access_token
  service_account_email   = var.build_service_account
  scopes                  = ["https://www.googleapis.com/auth/cloud-platform"]
  ssh_username            = "packer"
  tags                    = ["packer"]
}

build {
  sources = ["source.googlecompute.runner"]

  # Create /imagegeneration directory structure (matches official runner-images)
  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    inline          = ["mkdir ${var.image_folder}", "chmod 777 ${var.image_folder}"]
  }

  # Upload helpers to /imagegeneration/helpers/
  provisioner "file" {
    destination = "${var.helper_script_folder}"
    source      = "scripts/helpers"
  }

  # Upload installer scripts to /imagegeneration/installers/
  provisioner "file" {
    destination = "${var.installer_script_folder}"
    source      = "scripts/build"
  }

  # Upload toolset.json to /imagegeneration/installers/toolset.json
  provisioner "file" {
    destination = "${var.installer_script_folder}/toolset.json"
    source      = "${var.toolset_file}"
  }

  ###########################################################################
  # gcrunner-specific: Create runner user
  ###########################################################################
  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    script          = "scripts/gcrunner/configure-runner-user.sh"
  }

  ###########################################################################
  # APT configuration (matches official runner-images order)
  ###########################################################################
  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    script          = "scripts/build/configure-apt-mock.sh"
  }

  provisioner "shell" {
    environment_vars = ["HELPER_SCRIPTS=${var.helper_script_folder}", "DEBIAN_FRONTEND=noninteractive"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts          = ["scripts/build/configure-apt.sh"]
  }

  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    script          = "scripts/build/configure-limits.sh"
  }

  ###########################################################################
  # Environment configuration
  ###########################################################################
  provisioner "shell" {
    environment_vars = ["IMAGE_VERSION=${var.image_version}", "IMAGE_OS=${var.image_os}", "HELPER_SCRIPTS=${var.helper_script_folder}"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts          = ["scripts/build/configure-environment.sh"]
  }

  ###########################################################################
  # Vital packages
  ###########################################################################
  provisioner "shell" {
    environment_vars = ["DEBIAN_FRONTEND=noninteractive", "HELPER_SCRIPTS=${var.helper_script_folder}", "INSTALLER_SCRIPT_FOLDER=${var.installer_script_folder}"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts          = ["scripts/build/install-apt-vital.sh"]
  }

  ###########################################################################
  # Main installer scripts (matches official runner-images order, minus cut items)
  ###########################################################################
  provisioner "shell" {
    environment_vars = ["HELPER_SCRIPTS=${var.helper_script_folder}", "INSTALLER_SCRIPT_FOLDER=${var.installer_script_folder}", "DEBIAN_FRONTEND=noninteractive"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts = [
      "scripts/build/install-apt-common.sh",
      "scripts/build/install-git.sh",
      "scripts/build/install-git-lfs.sh",
      "scripts/build/install-github-cli.sh",
      "scripts/build/configure-dpkg.sh",
      "scripts/build/install-yq.sh",
      "scripts/build/install-zstd.sh",
    ]
  }

  ###########################################################################
  # Docker (separate step, matches official)
  ###########################################################################
  provisioner "shell" {
    environment_vars = ["HELPER_SCRIPTS=${var.helper_script_folder}", "INSTALLER_SCRIPT_FOLDER=${var.installer_script_folder}"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts          = ["scripts/build/install-docker.sh"]
  }

  ###########################################################################
  # gcrunner-specific: Runner agent
  ###########################################################################
  provisioner "shell" {
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    script           = "scripts/gcrunner/install-runner-agent.sh"
    environment_vars = ["RUNNER_VERSION=${var.runner_version}"]
  }

  ###########################################################################
  # gcrunner-specific: Cache server binary
  ###########################################################################
  provisioner "file" {
    source      = "../cache-server/cache-server"
    destination = "/tmp/cache-server"
  }

  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    inline = [
      "mv /tmp/cache-server /usr/local/bin/cache-server",
      "chmod +x /usr/local/bin/cache-server",
    ]
  }

  ###########################################################################
  # Pre-cache common GitHub Actions (~227 MB, saves download on every job)
  ###########################################################################
  provisioner "shell" {
    environment_vars = ["HELPER_SCRIPTS=${var.helper_script_folder}", "INSTALLER_SCRIPT_FOLDER=${var.installer_script_folder}"]
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    scripts          = ["scripts/build/install-actions-cache.sh"]
  }

  ###########################################################################
  # gcrunner-specific: Google Cloud CLI
  ###########################################################################
  provisioner "shell" {
    execute_command  = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    script           = "scripts/gcrunner/install-gcloud.sh"
  }

  ###########################################################################
  # Appwrite customisations: kernel pin, docker group, boot trim, preloads
  ###########################################################################
  provisioner "file" {
    source      = "appwrite"
    destination = "${var.image_folder}/appwrite"
  }

  provisioner "shell" {
    execute_command = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    inline          = ["bash ${var.image_folder}/appwrite/apply.sh", "rm -rf ${var.image_folder}/appwrite"]
  }

  ###########################################################################
  # Reboot (matches official)
  ###########################################################################
  provisioner "shell" {
    execute_command   = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    expect_disconnect = true
    inline            = ["echo 'Reboot VM'", "sudo reboot"]
  }

  ###########################################################################
  # Cleanup (after reboot, matches official)
  ###########################################################################
  provisioner "shell" {
    execute_command     = "sudo sh -c '{{ .Vars }} {{ .Path }}'"
    pause_before        = "1m0s"
    scripts             = ["scripts/build/cleanup.sh"]
    start_retry_timeout = "10m"
  }
}
