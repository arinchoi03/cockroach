provider "aws" {
  region = "${var.aws_region}"
}

resource "aws_instance" "cockroach" {
  tags {
    Name = "${var.key_name}-${count.index}"
  }
  ami = "${var.aws_ami_id}"
  availability_zone = "${var.aws_availability_zone}"
  instance_type = "${var.aws_instance_type}"
  security_groups = ["${aws_security_group.default.name}"]
  key_name = "${var.key_name}"
  count = "${var.num_instances}"
}

data "template_file" "supervisor" {
  count = "${var.num_instances}"
  template = "${file("supervisor.conf.tpl")}"
  vars {
    stores = "${var.stores}"
    # The value of the --join flag must be empty for the first node,
    # and a running node for all others. We built a list of addresses
    # shifted by one (first element is empty), then take the value at index "instance.index".
    join_address = "${element(concat(split(",", ""), aws_instance.cockroach.*.private_ip), count.index == 0 ? 0 : 1)}"
    # We need to provide one node address for the block writer.
    node_address = "${aws_instance.cockroach.0.private_ip}"
  }
}

# We use a null_resource to break the dependency cycle.
# This can be rolled back into aws_instance when https://github.com/hashicorp/terraform/issues/3999
# is addressed.
resource "null_resource" "cockroach-runner" {
  count = "${var.num_instances}"
  connection {
    user = "ubuntu"
    private_key = "${file(format("~/.ssh/%s.pem", var.key_name))}"
    host = "${element(aws_instance.cockroach.*.public_ip, count.index)}"
  }

  triggers {
    instance_ids = "${element(aws_instance.cockroach.*.id, count.index)}"
  }

  # This writes the filled-in supervisor template. It would be nice if we could
  # use rendered templates in the file provisioner.
  provisioner "remote-exec" {
    inline = <<FILE
echo '${element(data.template_file.supervisor.*.rendered, count.index)}' > supervisor.conf
FILE
  }

  # Launch CockroachDB.
  provisioner "remote-exec" {
    inline = [
      "sudo apt-get -y update",
      "sudo apt-get -y install supervisor",
      "sudo service supervisor stop",
      "mkdir -p logs",
      "chmod 755 cockroach",
      "[ $(stat --format=%s cockroach) -ne 0 ] || curl -sfSL https://edge-binaries.cockroachdb.com/cockroach/cockroach.linux-gnu-amd64.${var.cockroach_sha} -o cockroach",
      "chmod +x cockroach",
      "if [ ! -e supervisor.pid ]; then supervisord -c supervisor.conf; fi",
      "supervisorctl -c supervisor.conf start cockroach",
    ]
  }
}
