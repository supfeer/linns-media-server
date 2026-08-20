# Сценарий проверки

Запуск сервера:

    go run . -media "$PWD/test-data/library" -name "Linn Test"

После того как webOS покажет исходную библиотеку, в другом терминале добавьте файл:

    cp "$PWD/test-data/staging/New Movie.mp4" "$PWD/test-data/library/01 Movies/New Movie.mp4"

Проверка вложенной папки:

    mkdir -p "$PWD/test-data/library/02 Series/Season 2"
    cp "$PWD/test-data/staging/Nested Addition/Episode 02.mkv" "$PWD/test-data/library/02 Series/Season 2/Episode 02.mkv"

Сервер принимает изменение после двух одинаковых снимков. При стандартном интервале это занимает примерно 4–6 секунд. В журнале появится media library updated, после чего отправляются GENA SystemUpdateID/ContainerUpdateIDs и SSDP byebye/alive с прежними именем и UUID.

На webOS сначала подождите 6 секунд. Если открытый список не изменился, выйдите из папки и откройте ее снова. Перезагрузка телевизора не должна требоваться.

.hidden.mp4 и README.txt в DLNA отображаться не должны.
