# SoulX-Singer SVS adapter

This is the **SVS** workflow, not SoulX-Singer-SVC. It calls the verified upstream
`cli.inference.build_model`, `DataProcessor.process`, and `SoulXSinger.infer` APIs.
It does not run a preprocessor, download models, or turn plain target text into
aligned singing metadata.

Verified source: [Soul-AILab/SoulX-Singer, 81aeb3ae772c70093c3de74dc23c92d983801ae4](https://github.com/Soul-AILab/SoulX-Singer/tree/81aeb3ae772c70093c3de74dc23c92d983801ae4).
Model: [Soul-AILab/SoulX-Singer](https://huggingface.co/Soul-AILab/SoulX-Singer/tree/40493ad90286056c7a9095035164434a79daa8c9), **model.pt** (not `model-svc.pt`). Code and official model card specify Apache-2.0; respect author attribution and other dependency licenses. No upstream code or weights are bundled here.

## Provisioning and trust boundary

Provide a separate Python 3.10 environment with that upstream checkout's
`requirements.txt` installed (upstream pins torch/torchaudio 2.2.0), a complete
source checkout, and an existing directory containing the official SVS `model.pt`.
This adapter's own uv project depends only on Kanade's SDK. Offline Hugging Face
flags are set before the child starts; no `from_pretrained` or downloader is called.

**Use only trusted source code and model checkpoints.** Upstream `build_model`
loads its local checkpoint with `torch.load(weights_only=False)`, which can execute
pickle payloads. Kanade checks file presence, not provenance or checksum. Do not
rename an SVC or untrusted checkpoint to `model.pt`.

`device` defaults to `cuda`; `cpu` is an explicit, unbenchmarked option. The doctor
checks files, config, imports and requested CUDA availability, not weight integrity,
VRAM capacity, actual inference, or singing quality. No minimum GPU memory claim
has been verified.

## Inputs and editing

- `python_path`, `source_dir`, `model_dir`: local runtime paths.
- `prompt_audio`: clean authorized reference singing, **already cropped to the
  single prompt metadata segment**, starting at time 0.
- `prompt_metadata`: JSON array containing exactly one aligned prompt segment.
- `target_metadata`: JSON array of edited target singing segments. **Changing only
  the human-readable `text` field does not change singing.** Correct `phoneme`,
  `duration`, `note_pitch`, `note_type`, and (for melody control) `f0` to describe
  the new lyrics and their alignment, using the upstream preprocessing/MIDI-editor
  workflow with manual correction. Kanade does not guess phonemes or timing.
- `control`: `score` (default, MIDI note conditioning) or `melody` (F0 conditioning).
- `pitch_shift`: integer -24..24, default 0. `auto_shift` is always false so pitch
  is not silently moved toward the reference's register.

Each segment's `time` is `[start_ms,end_ms]`; phoneme/duration/note fields are
space-separated **strings**, with matching item counts. Durations are seconds.
F0 is a space-separated 50Hz sequence, required for both prompt and target in
melody mode. Segments must be sorted and non-overlapping. Validation permits at
most 120 seconds per segment, 1 hour of output, 1000 segments and 16MiB per JSON.
Prompt and target metadata are snapshotted when the SDK task begins, so a later
on-disk edit cannot change an already launched inference request.

`soulx.edit` generates `vocal.wav` at 24kHz, maintaining target segment offsets
with zero-filled gaps. Each generated segment is clipped to its explicit metadata
interval and shorter output is zero-padded; inspect timing and phrasing yourself.
There is no accompaniment mixing in this adapter; use the vocal output in a
separate mixing workflow. `soulx.doctor` needs only runtime paths and optional
`device`. Cancellation terminates the isolated child. Logs are `soulx.log` in the
job output directory. No exact timbre/melody fidelity is promised.

## Tests

From the Kanade root:

```sh
PYTHONPATH=engines/sdk/src:engines/soulx/src python -m unittest discover -s engines/soulx/tests -v
uv lock --check --offline --project engines/soulx
```

Tests use temporary fixtures, a real SDK handshake and mocked ML APIs. They are
not a real checkpoint/GPU/audio-quality acceptance test.
