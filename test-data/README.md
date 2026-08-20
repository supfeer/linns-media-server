# Verification scenario

Start the server:

    go run . -media "$PWD/test-data/library" -name "Linn Test"

After webOS displays the initial library, add a file from another terminal:

    cp "$PWD/test-data/staging/New Movie.mp4" "$PWD/test-data/library/01 Movies/New Movie.mp4"

Test a nested folder:

    mkdir -p "$PWD/test-data/library/02 Series/Season 2"
    cp "$PWD/test-data/staging/Nested Addition/Episode 02.mkv" "$PWD/test-data/library/02 Series/Season 2/Episode 02.mkv"

The server accepts a change after two identical scans. With the default interval, this takes approximately 4–6 seconds. The log then reports `media library updated`, followed by GENA `SystemUpdateID`/`ContainerUpdateIDs` events and an SSDP `byebye/alive` cycle using the existing name and UUID.

Wait 6 seconds on webOS. If the open list does not change, leave the folder and open it again. Restarting the television should not be necessary.

`.hidden.mp4` and `README.txt` must not appear in DLNA.
