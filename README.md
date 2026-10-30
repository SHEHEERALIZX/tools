# tools

go build -o branchcheck .

# inside a repo, CSV into the current directory
./branchcheck

# from anywhere, JSON, only pending branches, custom output folder
./branchcheck -repo /path/to/repo -format json -pending-only -out-dir ./reports -fetch

# excludes via list and file
./branchcheck -exclude "main,master,release/*" -exclude-file .branchignore
