# fly-image

Builds an image through Fly's remote builder and pushes it to the app's
registry namespace, inside the same apply that creates the machines that
run it. The label is a content hash of the files that go into the image, so
an apply rebuilds exactly when something that matters changed, and a
`fly_machine` that takes the module's `ref` waits for the push.

Used by [`../vault`](../vault/).
