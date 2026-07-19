# Speech-to-text support for Galene

Galene-stt is an implementation of real-time speech-to-text (automatic
subtitling) for the [Galene][1] videoconferencing server.  Depending on
how it is run, galene-stt may either produce a transcript of a conference,
or display captions in real time.

Galene-stt connects to a Galene server using the same protocol as any
other client, and may therefore be run on any machine that can connect to
the server.  This allows running galene-stt on a machine with a powerful
GPU without requiring a GPU to be available on the server.


## Installation

Build the Moonshine library:

```
git clone https://github.com/moonshine-ai/moonshine
cd moonshine/core
cmake -Bbuild
cd build
make -j
sudo mv libmoonshine.so /usr/local/lib
sudo ldconfig
```

Now download the Moonshine-mediuam streaming model:
```
pipx install moonshine-voice
moonshine-voice download --stt
```

Install the `libopus` library.  For example, under Debian, do
```
apt install libopus-dev
```

Build galene-stt:
```
git clone https://github.com/jech/galene-stt
cd galene-stt
ln -s ~/src/moonshine/core/moonshine-c-api.h .
CGO_ENABLED=1 go build -ldflags='-s -w'
```

## Usage

By default, galene-stt produces a transcript on standard output.  This
requires no special permissions, and may therefore be tested on any public
server:
```
./galene-stt https://galene.org:8443/group/public/stt/
```

In order to produce real-time captions, create a user called
`speech-to-text` with the `caption` permission in your Galene group:
```
galenectl create-group -group stt
galenectl create-user -group stt -user speech-to-text -permissions caption
galenectl set-password -group stt -user speech-to-text
```

Then run galene-stt with the `-caption` flag:
```
./galene-stt -caption https://galene.example.org:8443/group/stt/
```

Galene-stt defaults to english; for other languages, use the `-lang` flag:
```
./galene-stt -lang fr https://galene.example.org:8443/group/stt/
```

— Juliusz Chroboczek


[1]: https://galene.org
