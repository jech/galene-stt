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

Build and install whisper.cpp:

```
git clone https://github.com/ggml-org/whisper.cpp
cd whisper.cpp
cmake -Bbuild
cd build
make -j
sudo make install
sudo ldconfig
cd ..
```

Whisper.cpp does not scale well on the CPU, for production usage is is
necessary to run on a GPU.  If you have the CUDA compiler installed,
you can build with GPU support by replacing the third line with:
```
cmake -Bbuild -DGGML_CUDA=1
```
It is also possible to run whisper.cpp over *Vulkan* or (reportedly)
*Core ML*, please see the whisper.cpp documentation for details.

Now download your favourite model:
```
cd models
./download-ggml-model.sh medium
cd ../..
```

Install the `libopus` library.  For example, under Debian, do
```
apt install libopus-dev
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

By default, galene-stt produces a transcript on standard output:
```
./galene-stt https://galene.org:8443/group/public/stt/
```

In order to produce real-time captions, create a user called
`speech-to-text` with the `caption` permission in your Galene group:
```json
{
    "users": {
       "speech-to-text": {"permissions": "caption", "password": ...}
    }
}
```
Then run galene-stt with the `-caption` flag:
```
./galene-stt -caption https://galene.org:8443/group/public/stt/
```

Galene-stt defaults to english; for other languages, use the `-lang` flag:
```
./galene-stt -lang fr https://galene.org:8443/group/public/stt/
```

If galene-stt reports dropped audio, then your machine is not fast enough
for the selected model.  Specify a faster model using the `-model`
command-line option.  In my testing, however, models smaller than *medium*
did not produce useful output.

```
./galene-stt -caption -model models/ggml-tiny.bin \
             https://galene.org:8443/group/public/stt/
```

— Juliusz Chroboczek


[1]: https://galene.org
