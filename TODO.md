# TODO

## General

- add README
- cleanup logging, clear boxed logs
    [-] starting an operation
    [+] finished operation
    [*] error
    This makes it easy to filter the complete log with `grep grep "\[[-+*]\]" out.log` , to only show our output and ignore ffmpegs output.

## Production ready

- make sure evrything is set up for mounting (custom mount point created,)


## Long term

- make it distributed, assign one as the main server, distribute its queue task to its clients
    executable can stay the same, just start with either --client/--server

