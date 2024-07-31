# Speech-to-text support for Galene

Galene-stt is an experiment in real-time speech-to-text (automatic
subtitling) for the [Galene][1] videoconferencing server.  Currently
Galene-stt simply dumps a transcript of the videoconference in the chat;
if the experiment is successful, we will extend the Galene protocol with
the ability to display subtitles.

Galene-stt connects to a Galene server using the same protocol as ordinary
clients.  Since Galene-stt requires a fair amount of CPU, this allows
running on a powerful local machine without the risk to overload the
server.


## Installation

Build and install whisper.cpp:

```
git clone https://github.com/ggerganov/whisper.cpp
cd whisper.cpp
cmake -Bbuild
cd build
make -j
sudo make install
cd ..
```

Download your favourite model:
```
./models/download-ggml-model.sh base.en
cd ..
```

Build galene-stt:
```
git clone https://github.com/jech/galene-stt
cd galene-stt
go build
```

Put the models where galene-stt will find them:
```
ln -s ../whisper.cpp/models .
```


## Usage

```
./galene-stt https://galene.org:8443/group/public/stt
```

You may specify a specific model using the `-model` command-line option:

```
./galene-stt -model models/ggml-tiny.bin \
             https://galene.org:8443/group/public/stt
```


Type `./galene-stt -help` for more information.


— Juliusz Chroboczek


[1]: https://galene.org
