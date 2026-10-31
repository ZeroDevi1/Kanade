# Vevo2 AR + FM adapter

This is **lyrics editing**, not the FM-only VC/SVC route. It follows upstream
`vevo2_editing`: the original clean vocal is used for prosody, style and timbre;
new text goes through the autoregressive model and then flow matching + vocoder.
Output is 24 kHz `vocal.wav`, optionally `mixed.wav`. Melody, pronunciation,
duration and timbre are approximate model outputs. Upstream generation caps AR
new tokens at 500; use short clips, not an assumed whole-song editing workflow.

Verified source: Amphion commit
`26f6883110181f1dbfe95c70a7c7dbaf4de5f42a`, files
`models/svc/vevo2/infer_vevo2_ar.py` and `vevo2_utils.py`.
Verified model layout: `RMSnow/Vevo2` snapshot
`2674843cbaa50aa89ee7ccaf5bb15d6ccf46c6c8`.

## Preprovisioned environment

Use a separate Python 3.10 environment and complete Amphion source checkout.
The three packages in `models/svc/vevo2/requirements.txt` are **not** all of its
imports: the full Amphion ML dependencies are also required. Do not replace the
Kanade RVC environment. Upstream base environment pins and the newer Vevo2/Qwen
requirements may need compatible versions; this adapter's mock tests are not a
validated torch/transformers/GPU installation recipe.

Select local `python_path`, `source_dir`, `model_dir`, and `whisper_path` (an
existing OpenAI Whisper **medium.pt**). `model_dir` uses the official layout:

- `contentstyle_modeling/posttrained/`: AR `model.safetensors`, `config.json`,
  `amphion_config.json`, `tokenizer.json`, `tokenizer_config.json`,
  `added_tokens.json`, `special_tokens_map.json`, `merges.txt`, `vocab.json`.
- `acoustic_modeling/fm_emilia101k_singnet7k_repa/`: `model.safetensors`,
  `config.json`, `whisper_stats.pt`.
- `tokenizer/contentstyle_fvq16384_12.5hz/model.safetensors`.
- `tokenizer/prosody_fvq512_6.25hz/model.safetensors`.
- `vocoder/config.json`, `vocoder/model.safetensors`.

The adapter never calls upstream `snapshot_download` or the official auto-loader.
Within the disposable process only, it redirects Whisper's hardcoded `medium`
loader to `whisper_path`, loads the built-in Qwen2 architecture with
`local_files_only=True, trust_remote_code=False`, and rewrites the JSON5 FM config's
hardcoded Whisper statistics path to the local snapshot file (optional explicit
`whisper_stats_path` override). Non-default text-conditioned FM configs are
rejected rather than fetching an external tokenizer. Original source files and
configuration are never rewritten. HF offline flags are set before ML imports.

`vevo2.doctor` checks the local layout, imports and JSON5 config, **not** checkpoint
contents, inference quality, VRAM sufficiency or a real forward pass.
`vevo2.edit` additionally needs `source_audio`, `original_text`, `target_text`.
Optional `accompaniment_audio` is resampled and zero-padded, mixed from time zero,
with global peak normalization if clipping would occur; no automatic alignment.
Advanced options: integer `seed` 0–2147483647 (default 0),
`flow_matching_steps` 1–128 (default 32). Lyrics are passed via temporary JSON,
not process arguments. Cancellation terminates the worker. Failures leave
`vevo2.log` in the job output directory.

## Licensing

Amphion code is MIT. **Vevo2 model weights are CC-BY-NC-ND 4.0**:
https://huggingface.co/RMSnow/Vevo2 . Read and comply with the license, including
attribution, non-commercial and no-derivatives terms; this integration is not
commercial permission and does not interpret licensing of generated outputs.
Both doctor and edit require `license_accepted: true` to acknowledge the notice.
No weights or third-party code are bundled in this adapter. Separately check the
Whisper and dependency licenses and the rights to the input recording/voice.

## Dependency-free adapter checks

From repository root:

```sh
PYTHONPATH=engines/sdk/src:engines/vevo2/src python -m unittest discover -s engines/vevo2/tests -v
uv lock --check --offline --project engines/vevo2
```

Tests use mocked ML modules and temporary checkpoint fixtures, plus the real SDK
subprocess protocol. They do not download weights or validate generated audio.
