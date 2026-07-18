variable "region" {
  type = string
}

resource "aws_instance" "web" {
  ami = "ami-example"
}

module "network" {
  source = "./network"
}
