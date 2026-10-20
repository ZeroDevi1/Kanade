# Stems（分轨）模型评估

日期：2026-10-20。环境：Windows 11、RTX 5060 8 GB、audio-separator 0.47.1、torch 2.14.1+cu130。
参数与净化链一致：`segment_size=256`、`overlap=4`、`batch_size=1`。评估脚本不在仓库中（一次性实验）。

## 结论

| 步骤 | 默认模型（与 VoiceLab 一致） | 评估最优 | 依据 |
|---|---|---|---|
| 伴奏 | `melband_roformer_inst_v1e` | `model_bs_roformer_ep_317_sdr_12.9755`（BS-Roformer-Viperx-1297） | 伴奏 SDR 14.55 vs 13.77 |
| 干声 | `melband_roformer_big_beta5e` | `BS-Roformer-SW` | 人声 SDR 10.86 vs 10.27；无人声段残留低 26 dB；更快、显存更小 |
| 去和声 | `mel_band_roformer_karaoke_aufr33_viperx_sdr_10.1956` | `mel_band_roformer_karaoke_gabox_v2` | 合成集 SDR 9.11 vs 8.48（仅近似评估） |
| 去混响 | `dereverb_mel_band_roformer_anvuew_sdr_19.1729` | 同默认 | 合成集最优 |
| 降噪 | `denoise_mel_band_roformer_aufr33_sdr_27.9959` | 同默认 | 未单独评估 |

默认链未修改；`stems.chain` 新增 `models` 参数可按步骤覆盖模型（翻唱测试用的就是上表「评估最优」一列）。
是否把默认值换成评估最优，需要结合主观试听决定：big_beta5e 等「fullness」取向的模型本就不以 SDR 为目标。

## 1. 人声 / 伴奏分离（MUSDB18 测试集，有真值）

数据：MUSDB18 7 秒片段版测试集 50 首（Zenodo 3270814，AAC 压缩，适合相对比较）。
50 段拼成一条（段间 1 秒静音）分离一次；SDR = 10·log10(Σs² / Σ(s−ŝ)²)，逐首计算后取均值。

| 模型 | 人声 SDR 均值 | 人声 SDR 中位数 | RTF | 峰值显存 |
|---|---|---|---|---|
| BS-Roformer-Viperx-1297 | **10.91** | 11.39 | 0.81 | 1.60 GB |
| BS-Roformer-SW（6 stem） | 10.86 | 11.37 | 0.45 | 1.46 GB |
| Kim Vocals（`vocals_mel_band_roformer`） | 10.59 | 11.00 | 0.37 | 1.99 GB |
| Kim FT2 Bleedless（unwa） | 10.44 | 11.00 | 0.69 | 1.69 GB |
| big_beta5e（默认干声） | 10.27 | 10.68 | 0.40 | 2.22 GB |
| Vocals（becruily） | 10.23 | 10.89 | 0.38 | 1.99 GB |
| Vocals FV4（Gabox） | 10.15 | 10.80 | 0.39 | 1.99 GB |
| MDX23C-InstVoc HQ | 10.07 | 10.68 | 0.30 | 1.60 GB |
| inst_v1e（默认伴奏） | 9.45 | 9.69 | 0.36 | 1.99 GB |
| htdemucs_ft | 8.43 | 8.42 | 0.22 | 1.04 GB |

直接输出伴奏的两 stem 模型的伴奏 SDR：BS-Roformer-Viperx-1297 14.55，inst_v1e 13.77。
（只输出人声的模型以「原曲 − 人声」作伴奏，口径不同，不列入比较。）
Vocals Resurrection（unwa）在 6 首抽查中人声 SDR 只有 6.17，未做全量评估。

## 2. 目标歌曲上的串音检查（无真值）

《如诗一般的形容妳》（48 kHz / 24 bit FLAC）。按 `.lrc` 时间轴取前奏、间奏、尾奏（无人声）与两段主歌副歌，
比较人声轨在两类段落的 RMS：无人声段的残留即乐器串音。

| 模型 | 演唱段 | 无人声段 | 差值 |
|---|---|---|---|
| BS-Roformer-SW | −15.5 dB | **−92.4 dB** | 76.9 dB |
| becruily | −15.3 dB | −75.2 dB | 59.9 dB |
| big_beta5e | −15.2 dB | −66.7 dB | 51.5 dB |
| BS-Roformer-Viperx-1297 | −15.5 dB | −63.2 dB | 47.7 dB |
| Kim FT2 Bleedless | −15.3 dB | −60.5 dB | 45.2 dB |
| Kim Vocals | −15.4 dB | −52.6 dB | 37.2 dB |

各模型的残留都远低于可闻程度，差异主要在不可闻区间；它与 MUSDB 结果一起支持干声选 SW。

完整净化链对比（同一首歌）：伴奏与人声在演唱段的相关系数，默认链 0.0458，评估最优组合 0.0123，
即伴奏中残留的原唱约为默认链的 1/4；最终干声在无人声段分别为 −93.2 dB 与 −90.9 dB。

另：该 FLAC 在 265.47 秒（尾奏淡出的最后 1.4 秒）有一帧损坏（ffmpeg：`invalid sync code`），解码时跳过约 85 ms，不影响使用。

## 3. 去和声、去混响（合成近似集）

没有公开的真值数据，用合成数据近似，结论只作参考：

- 去和声：主唱 = MUSDB 人声真值，和声 = 另一首的人声 −6 dB、右声道延迟 15 ms，共 20 段；目标为主唱。
  真实和声与主唱通常是同一歌手，这里是不同歌手，偏向能区分音色的模型。
- 去混响：干声 = 炫狗数据集的 20 段说话录音；指数衰减噪声脉冲响应（RT60 0.8–2.0 s，左右独立）、湿声 35–60%；
  目标为干声部分。没有早期反射，与真实房间混响不同。

| 去和声模型 | SDR（输入 5.92） | | 去混响模型 | SDR（输入 1.07） |
|---|---|---|---|---|
| Karaoke V2（Gabox） | **9.11** | | anvuew（默认） | **11.09** |
| Karaoke（becruily） | 8.91 | | anvuew mono | 11.08 |
| aufr33-viperx（默认） | 8.48 | | anvuew less aggressive | 10.95 |
| BS Karaoke（frazer-becruily） | 7.83 | | Big（Sucial） | 8.26 |
| BS Karaoke（anvuew） | 6.01 | | BS-Roformer De-Reverb | 2.01 |
| | | | De-Reverb-Echo V2（Sucial） | 1.07 |

## 评估中遇到的问题

- 评估脚本最初按文件名是否含 `_vocals` 判断 stem，`vocals_mel_band_roformer` 这类模型名本身含 vocals，导致误判为 −5 dB；
  改为只看 audio-separator 输出名括号中的 stem 名后正常。`stems.chain` 的模型覆盖同样按 stem 名的角色匹配，不看文件名。
- 分离与训练同时占用 GPU 时，显存接近 8 GB 后 Windows 驱动回退到共享内存，RVC 训练从 1.3 秒/步降到 8 秒/步。
  8 GB 显卡上分轨、训练应依次进行。
