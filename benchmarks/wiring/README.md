# What wiring the GraphQL layer costs

The scripts behind the table in [`docs/large-schemas.md`](../../docs/large-schemas.md)
("Wiring the GraphQL layer"). They write a synthetic service of N entities --
a service package and a gqlc group package each, no ORM -- in one of three
layouts, and time its builds and its start-up.

| Layout | Handlers are constructed by | A handler's constructor takes |
|---|---|---|
| `fx` | uber/fx, one registration per group | its own service |
| `deps` | a hand-written `Wire` | the whole `Services` struct |
| `field` | gqlc's generated `Wire` | its own service, `d.<Entity>` |

`deps` is written by hand because gqlc no longer generates it: it hands a
group the field named after it. It is kept as the thing to compare against.

```sh
go build -o /tmp/gqlc ../../cmd/gqlc
repo=$(cd ../.. && pwd)
for layout in fx deps field; do
  python3 gen.py $layout 300 /tmp/wiring-$layout "$repo" /tmp/gqlc
  sh measure.sh /tmp/wiring-$layout /tmp/wiring-cache-$layout
done
```

`measure.sh` prints, for each build, wall time, CPU time, the peak memory of
the largest process, and how many of the service's own packages compiled: a
cold build, then three edits each of a service's body, a service's exported
methods, a handler, and the `Services` struct. It ends with the start-up
figures the generated server reports about itself.

Give every layout a build cache of its own (the second argument), or the
first one measured pays for the standard library and the others do not.
Warm each with the dependencies first if the cold figure matters -- build a
five-entity service into it. Rely on the package counts, the CPU time and the
allocation counts: on the laptop these were taken on, wall time for the same
build moved by half from one run to the next. For start-up time, run the
three servers alternately rather than one after another.

`measure.sh` uses `/usr/bin/time -l` and `sed -i ''`, which are macOS's
spellings; on Linux they are `/usr/bin/time -v` and `sed -i`.
