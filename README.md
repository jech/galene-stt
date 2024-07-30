# Speech-to-text support for Galene

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

Run galene-stt:
```
./galene-stt https://galene.org:8443/group/public/stt
```

Type `./galene-stt -help` for more information.


— Juliusz Chroboczek
