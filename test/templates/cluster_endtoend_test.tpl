name: {{.Name}}
on: [push, pull_request]
concurrency:
  group: format('{0}-{1}', ${{"{{"}} github.ref {{"}}"}}, '{{.Name}}')
  cancel-in-progress: true

jobs:
  build:
    name: Run endtoend tests on {{.Name}}
    {{if .Ubuntu20}}runs-on: ubuntu-20.04{{else}}runs-on: ubuntu-18.04{{end}}

    steps:
    - name: Set up Go
      uses: actions/setup-go@v1
      with:
        go-version: 1.16

    - name: Tune the OS
      run: |
        echo '1024 65535' | sudo tee -a /proc/sys/net/ipv4/ip_local_port_range

    # TEMPORARY WHILE GITHUB FIXES THIS https://github.com/actions/virtual-environments/issues/3185
    - name: Add the current IP address, long hostname and short hostname record to /etc/hosts file
      run: |
        echo -e "$(ip addr show eth0 | grep "inet\b" | awk '{print $2}' | cut -d/ -f1)\t$(hostname -f) $(hostname -s)" | sudo tee -a /etc/hosts
    # DON'T FORGET TO REMOVE CODE ABOVE WHEN ISSUE IS ADRESSED!

    - name: Check out code
      uses: actions/checkout@v2

    - name: Get dependencies
      run: |
        sudo apt-get update
        sudo apt-get install -y mysql-server mysql-client make unzip g++ etcd curl git wget eatmydata
        sudo service mysql stop
        sudo service etcd stop
        sudo ln -s /etc/apparmor.d/usr.sbin.mysqld /etc/apparmor.d/disable/
        sudo apparmor_parser -R /etc/apparmor.d/usr.sbin.mysqld
        go mod download

        {{if .InstallXtraBackup}}

        wget https://repo.percona.com/apt/percona-release_latest.$(lsb_release -sc)_all.deb
        sudo apt-get install -y gnupg2
        sudo dpkg -i percona-release_latest.$(lsb_release -sc)_all.deb
        sudo apt-get update
        sudo apt-get install percona-xtrabackup-24

        {{end}}

    {{if .MakeTools}}

    - name: Installing zookeeper and consul
      run: |
          make tools

    {{end}}

    - name: Run cluster endtoend test
      timeout-minutes: 30
      run: |
        source build.env
        eatmydata -- go run test.go -docker=false -print-log -follow -shard {{.Shard}}
