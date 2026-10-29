"""CPU-only workflow tests. No pretrained weights, torch or network required."""
import json
import tempfile
import unittest
from dataclasses import asdict
from pathlib import Path
from unittest.mock import patch

import numpy as np
import soundfile as sf

from kanade_timbre import bwe, runtime, train, training_data as td


class Job:
    def __init__(self, root, params):
        self.params, self.output_dir, self.id = params, root / "outputs", "test"
        self.fraction, self.cancelled, self.artifacts = 0, False, []
    def progress(self, f, message=""):
        self.fraction = f
    def check_cancelled(self):
        pass
    def artifact(self, path, kind="file"):
        self.artifacts.append((Path(path), kind))


class DataTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.src = self.root / "source"
        self.src.mkdir()
        self.wav = self.src / "a.wav"
        sf.write(self.wav, np.random.default_rng(1).normal(0, .05, 24000), 24000)
    def tearDown(self):
        self.tmp.cleanup()
    def test_default_off_and_validation(self):
        self.assertEqual(td.options({})["mode"], "off")
        for p in ({"bwe_mode":"bad"}, {"bwe_mode":"manual","bwe_cutoff_hz":1999},
                  {"bwe_mode":"manual","bwe_cutoff_hz":15000}, {"bwe_cutoff_hz":float("nan")}):
            with self.assertRaises(ValueError): td.options(p)
    def test_whitelist_and_sample_rate(self):
        sf.write(self.src / "unlisted.wav", np.zeros(4800), 48000)
        (self.src / "source.list").write_text("a.wav|speaker|zh|test\n")
        files = td.selected_files(self.src)
        self.assertEqual(files, [self.wav])
        self.assertEqual(td.source_sample_rate(files), 32000)
        (self.src / "source.list").write_text("missing.wav|x|zh|x")
        with self.assertRaises(ValueError): td.selected_files(self.src)
    def test_content_and_model_changes_invalidate_signature(self):
        opts = td.options({})
        first, _ = td.fingerprint([self.wav], opts, {}, [])
        self.wav.write_bytes(self.wav.read_bytes() + b"x")
        changed, _ = td.fingerprint([self.wav], opts, {}, [])
        self.assertNotEqual(first, changed)
        weights = self.root / "weights" / "12kto48k"
        weights.mkdir(parents=True)
        for f in ("config.json", "g_12kto48k.safetensors"): (weights / f).write_text("one")
        old = bwe.model_signatures(weights.parent, {weights.name})
        (weights / "g_12kto48k.safetensors").write_text("two")
        self.assertNotEqual(old, bwe.model_signatures(weights.parent, {weights.name}))
    def test_resume_guards(self):
        exp = self.root / "exp"; exp.mkdir()
        meta = {"data_fingerprint":"same","data_ready":True}
        td.guard_resume(exp, meta, "same", True, True)
        for m, sig, sp, se in [(meta,"changed",False,False), ({},"same",True,True),
                                ({**meta,"data_ready":False},"same",True,False), (meta,"same",False,True)]:
            with self.assertRaises(ValueError): td.guard_resume(exp,m,sig,sp,se)
        (exp / "G_100.pth").touch()
        with self.assertRaises(ValueError): td.guard_resume(exp,{},"same",False,False)
    def test_clear_cache_preserves_sources_and_checkpoints(self):
        exp = self.root / "exp";exp.mkdir()
        for n in ("sliced_audios","f0","f0_voiced","extracted","_dataset_bwe"):
            (exp/n).mkdir();(exp/n/"old").touch()
        (exp/"G_100.pth").touch()
        td.clear_prepared(exp, True)
        self.assertFalse((exp/"extracted").exists())
        self.assertFalse((exp/"sliced_audios").exists())
        self.assertTrue((exp/"_dataset_bwe"/"old").exists())
        self.assertTrue((exp/"G_100.pth").exists())
        self.assertTrue(self.wav.exists())
    def test_copy_does_not_alias_original(self):
        dst=self.root/"copy.wav";bwe._link(self.wav,dst)
        before=self.wav.read_bytes();dst.write_bytes(b"changed")
        self.assertEqual(before,self.wav.read_bytes())
        with self.assertRaises(ValueError): bwe._link(self.wav,self.wav)
    def test_colliding_stems_have_unique_outputs(self):
        files=[]
        for n in ("a.wav","a.mp3","a_1.wav","A.wav"):
            f=self.src/n
            if not f.exists(): f.write_bytes(self.wav.read_bytes())
            files.append(f)
        items=bwe.plan(files,self.root/"out")
        self.assertEqual(len({i.out.casefold() for i in items}),len(files))
    def test_bwe_cache_and_atomic_source_preservation(self):
        src_before=self.wav.read_bytes()
        out=self.root/"output/a.wav";out.parent.mkdir()
        # Simulate a legacy output hardlinked to source: replacement must break that link.
        import os
        os.link(self.wav,out)
        item=bwe.Item(str(self.wav),str(out),7000,"12kto48k",self.wav.stat().st_size,self.wav.stat().st_mtime_ns)
        plan=self.root/"plan.json";plan.write_text(json.dumps([asdict(item)]))
        d=self.root/"models"/item.model;d.mkdir(parents=True)
        (d/"config.json").write_text("{}")
        weights=d/f"g_{item.model}.safetensors";weights.write_text("one")
        (out.parent/"unrelated.txt").write_text("keep")
        with patch.object(bwe,"_device",return_value="cpu"),patch.object(bwe,"_Generator"), \
             patch.object(bwe,"extend",side_effect=lambda g,x,b:x) as extend:
            bwe.run(plan,d.parent);self.assertEqual(extend.call_count,1)
            bwe.run(plan,d.parent);self.assertEqual(extend.call_count,1)
            weights.write_text("two")
            bwe.run(plan,d.parent);self.assertEqual(extend.call_count,2)
        self.assertEqual(src_before,self.wav.read_bytes())
        self.assertTrue((out.parent/"unrelated.txt").exists())

    def test_manual_plan(self):
        items=td.make_plan([self.wav],self.root/"out",td.options({"bwe_mode":"manual","bwe_cutoff_hz":7000}))
        self.assertEqual(items[0].model,"12kto48k")
        self.assertEqual(items[0].bandwidth,7000)
    def test_missing_subprocess_output_blocks_training(self):
        item=bwe.Item(str(self.wav),str(self.root/"missing.wav"),7000,"12kto48k",1,1)
        with self.assertRaises(RuntimeError): td.process(Job(self.root,{}),[item],self.root/"plan.json",lambda *a,**k:None,0,1)
    def test_preview_bounded_outputs_no_training(self):
        item=bwe.Item(str(self.wav),str(self.root/"outputs/enhanced/a.wav"),7000,"12kto48k",1,1)
        job=Job(self.root,{"dataset_dir":str(self.src),"bwe_mode":"auto","preview_count":3})
        def process(job,items,*args):
            for it in items:
                p=Path(it.out);p.parent.mkdir(parents=True,exist_ok=True);sf.write(p,np.zeros(4800),48000)
        with patch.object(td,"make_plan",return_value=[item]),patch.object(runtime,"ensure"),patch.object(td,"process",side_effect=process):
            td.preview(job,lambda *a,**k: self.fail("no trainer"))
        self.assertEqual([p.name for p,k in job.artifacts],["original-01.wav","enhanced-01.wav","bandwidth-report.json"])
        self.assertEqual(json.loads((job.output_dir/"bandwidth-report.json").read_text())["previewed"],[str(self.wav)])
    def test_preview_zero_candidates_and_limits(self):
        job=Job(self.root,{"dataset_dir":str(self.src),"bwe_mode":"auto","preview_count":6})
        with self.assertRaises(ValueError): td.preview(job,None)
        job.params["preview_count"]=3
        with patch.object(td,"make_plan",return_value=[]),patch.object(runtime,"ensure") as ensure:
            td.preview(job,None)
            ensure.assert_not_called()
        self.assertEqual(len(job.artifacts),1)

    def test_extracted_requires_f0_and_embeddings(self):
        exp=self.root/"exp";exp.mkdir()
        for d in ("sliced_audios","extracted","f0","f0_voiced"): (exp/d).mkdir()
        (exp/"sliced_audios/a.wav").touch();(exp/"extracted/a.npy").touch()
        with self.assertRaises(RuntimeError): train.check_extracted(exp)
        for d in ("f0","f0_voiced"): (exp/d/"a.wav.npy").touch()
        train.check_extracted(exp)
    def test_staging_excludes_unfingerprinted_nested_audio(self):
        nested=self.src/"0";nested.mkdir();(nested/"hidden.wav").write_bytes(self.wav.read_bytes())
        out=td.stage_selected(td.selected_files(self.src),self.root/"staged")
        self.assertEqual([p.name for p in out.iterdir()],["a.wav"])
        with self.assertRaises(ValueError): td.stage_selected([self.wav],self.src)
    def test_training_wires_optional_bwe_and_rejects_changed_resume(self):
        for mode in ("off","manual"):
            with self.subTest(mode=mode):
                root=self.root/mode;root.mkdir()
                log=root/"logs";log.mkdir()
                job=Job(root,{"name":"test","dataset_dir":str(self.src),"sample_rate":48000,
                              "epochs":2,"batch_size":1,"bwe_mode":mode,"bwe_cutoff_hz":7000})
                exp=log/"test"
                stages=[]
                def ensure(job, assets, *args):
                    for asset in assets:
                        dest=root/asset.local;dest.parent.mkdir(parents=True,exist_ok=True);dest.write_text("mock")
                def scripts(job,args,*rest,**kwargs):
                    if "kanade_timbre.bwe" in args:
                        stages.append("bwe")
                        for item in json.loads(Path(args[2]).read_text()):
                            dest=Path(item["out"]);dest.parent.mkdir(parents=True,exist_ok=True)
                            sf.write(dest,np.zeros(4800),48000)
                    elif args[0].endswith("preprocess.py"):
                        stages.append("preprocess")
                        self.assertIn("_dataset_bwe" if mode=="manual" else "_dataset_input",args[2])
                        (exp/"sliced_audios").mkdir();sf.write(exp/"sliced_audios/a.wav",np.zeros(4800),48000)
                    elif args[0].endswith("extract.py"):
                        stages.append("extract")
                        for d,name in (("extracted","a.npy"),("f0","a.wav.npy"),("f0_voiced","a.wav.npy")):
                            (exp/d).mkdir();np.save(exp/d/name,np.zeros(5))
                    elif args[0].endswith("train.py"):
                        stages.append("train");(exp/"test_2e_10s.pth").touch()
                    else: stages.append("index")
                with patch.object(runtime,"ROOT",root),patch.object(runtime,"LOGS",log),patch.object(runtime,"ensure",side_effect=ensure), \
                     patch.object(train,"run_script",side_effect=scripts),patch.object(train.gpu,"query",return_value=None),patch.object(train,"TrainMonitor") as monitor:
                    monitor.return_value.peak = None
                    train.run_train(job)
                    self.assertEqual(stages,(["bwe"] if mode=="manual" else [])+["preprocess","extract","train","index"])
                    meta=json.loads((exp/train.TRAIN_META).read_text());self.assertTrue(meta["data_ready"])
                    job.params.update({"epochs":4,"skip_preprocess":True,"skip_extract":True})
                    stages.clear();train.run_train(job)
                    self.assertEqual(stages,["train","index"])
                    job.params.update({"bwe_mode":"auto"})
                    with self.assertRaises(ValueError): train.run_train(job)


if __name__ == "__main__":
    unittest.main()
