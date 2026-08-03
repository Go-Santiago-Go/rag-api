# infra/network.tf
# The multi-tier VPC for go-rag-api: public, private-app, and private-data
# subnets across two AZs. Built in three logical passes below:
#   1. VPC + subnets   (the address space)
#   2. IGW + NAT        (the doors to the internet)
#   3. Route tables     (what actually makes a subnet public or private)
#
# Design note: a subnet is only a CIDR slice pinned to one AZ. What makes it
# "public" or "private" is its route table (pass 3), not the subnet itself.
# We run a SINGLE NAT gateway in one AZ (not one per AZ). That trades
# availability for ~$32/mo of savings, an intentional choice for a demo we
# tear down nightly, not an oversight.

# ---------------------------------------------------------------------------
# Pass 1: VPC + subnets
# ---------------------------------------------------------------------------

# The VPC: a private /16 address space we fully own (~65k addresses).
resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true # let instances resolve DNS (needed to reach AWS APIs)
  enable_dns_hostnames = true # required so the RDS instance gets a resolvable endpoint

  tags = { Name = "go-rag-api" }
}

# Look up the region's AZs instead of hardcoding "us-east-1a", so the config
# survives a region change. Consumed below as .names[0] and .names[1].
data "aws_availability_zones" "available" {
  state = "available"
}

# --- Public tier: reachable from the internet. Hosts the Express Mode load
# balancer, the NAT gateway, and, because ECS Express Mode couples the load
# balancer and tasks into one subnet set, the ECS tasks themselves. ---
# map_public_ip_on_launch gives resources here a public IP. This is one
# ingredient of "public"; the internet route in pass 3 is the other.
resource "aws_subnet" "public_a" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.0.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
  tags                    = { Name = "go-rag-api-public-a", Tier = "public" }
}

resource "aws_subnet" "public_b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[1]
  map_public_ip_on_launch = true
  tags                    = { Name = "go-rag-api-public-b", Tier = "public" }
}

# --- App tier: private, outbound-only via NAT. Designed to hold the Fargate
# task, but ECS Express Mode places the service in the PUBLIC subnets (it uses
# one subnet set for both the load balancer and the tasks, and a public URL
# needs public subnets). So these subnets and the NAT are provisioned by the
# multi-tier design but currently sit off the request path, the point where a
# hand-rolled aws_ecs_service would put tasks to keep them fully private. ---
# No public IP (map_public_ip_on_launch defaults to false).
resource "aws_subnet" "app_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.10.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]
  tags              = { Name = "go-rag-api-app-a", Tier = "app" }
}

resource "aws_subnet" "app_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.11.0/24"
  availability_zone = data.aws_availability_zones.available.names[1]
  tags              = { Name = "go-rag-api-app-b", Tier = "app" }
}

# --- Data tier: private isolated, holds RDS. No internet route at all. ---
# RDS requires a subnet group spanning two AZs even for a single-AZ instance,
# which is why this tier is a pair even though only one AZ runs the DB.
resource "aws_subnet" "data_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.20.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]
  tags              = { Name = "go-rag-api-data-a", Tier = "data" }
}

resource "aws_subnet" "data_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.21.0/24"
  availability_zone = data.aws_availability_zones.available.names[1]
  tags              = { Name = "go-rag-api-data-b", Tier = "data" }
}

# ---------------------------------------------------------------------------
# Pass 2: the doors to the internet (IGW + NAT)
# ---------------------------------------------------------------------------

# Internet gateway: the TWO-WAY door. Attached to the VPC, it lets public
# resources send traffic out AND lets the internet initiate connections in.
resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "go-rag-api-igw" }
}

# A NAT gateway needs a stable public IP. domain = "vpc" allocates an Elastic
# IP scoped to this VPC (the modern replacement for the deprecated vpc = true).
resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = { Name = "go-rag-api-nat-eip" }

  # The EIP is only useful once the VPC has an internet gateway attached.
  depends_on = [aws_internet_gateway.main]
}

# NAT gateway: the ONE-WAY door. It lives in a PUBLIC subnet and lets private
# subnets reach out to the internet while blocking any inbound connection. It
# was the egress path for the app when the task was meant to live in the private
# app tier; with Express Mode running the task in public subnets, the app now
# egresses straight through the internet gateway, so the NAT is currently unused
# and a candidate for removal. Single NAT in one AZ, per the design note above.
resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public_a.id
  tags          = { Name = "go-rag-api-nat" }

  depends_on = [aws_internet_gateway.main]
}

# ---------------------------------------------------------------------------
# Pass 3: route tables (what actually makes a subnet public vs private)
# ---------------------------------------------------------------------------
# Every route table has an implicit "local" route for the VPC CIDR, so all
# subnets can always talk to each other. The 0.0.0.0/0 (default) route is what
# differs per tier, and that difference IS the public/private distinction.

# Public route table: default route to the INTERNET GATEWAY. Associating a
# subnet here is what makes it public.
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "go-rag-api-rt-public" }
}

resource "aws_route_table_association" "public_a" {
  subnet_id      = aws_subnet.public_a.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "public_b" {
  subnet_id      = aws_subnet.public_b.id
  route_table_id = aws_route_table.public.id
}

# App route table: default route to the NAT GATEWAY. Outbound internet works,
# inbound does not. Both app subnets share this one table (and thus the single
# NAT), which is the concrete point where the single-NAT trade-off lives:
# if the NAT's AZ fails, both AZs lose outbound.
resource "aws_route_table" "app" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main.id
  }

  tags = { Name = "go-rag-api-rt-app" }
}

resource "aws_route_table_association" "app_a" {
  subnet_id      = aws_subnet.app_a.id
  route_table_id = aws_route_table.app.id
}

resource "aws_route_table_association" "app_b" {
  subnet_id      = aws_subnet.app_b.id
  route_table_id = aws_route_table.app.id
}

# Data route table: NO default route. Only the implicit local route exists, so
# the data subnets can be reached from inside the VPC but have no path to or
# from the internet. This absence of a route is what truly isolates RDS,
# the security group is a second, separate layer on top.
resource "aws_route_table" "data" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "go-rag-api-rt-data" }
}

resource "aws_route_table_association" "data_a" {
  subnet_id      = aws_subnet.data_a.id
  route_table_id = aws_route_table.data.id
}

resource "aws_route_table_association" "data_b" {
  subnet_id      = aws_subnet.data_b.id
  route_table_id = aws_route_table.data.id
}
