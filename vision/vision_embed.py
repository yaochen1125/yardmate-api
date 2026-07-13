"""BioCLIP-2 嵌入(P0 选定模型)。单例懒加载。给 build_index 和 app 共用。"""
import io, gc, warnings, threading
warnings.filterwarnings("ignore")
import torch, torch.nn.functional as F
from PIL import Image
import open_clip

MODEL_ID = "hf-hub:imageomics/bioclip-2"
_lock = threading.Lock()
_state = {"model": None, "pp": None, "dev": None, "dim": None}

def _ensure():
    if _state["model"] is None:
        with _lock:
            if _state["model"] is None:
                dev = "mps" if torch.backends.mps.is_available() else "cpu"
                m, _, pp = open_clip.create_model_and_transforms(MODEL_ID)
                m.eval().to(dev)
                # C1: 只用 encode_image。释放未用的文本塔省 ~440MB(实测释放后嵌入 bit-identical),
                # 让 8G box 上 API(4G)+serve 常驻不吃紧、避免 OOM 误杀 API。
                for attr in ("transformer", "token_embedding", "ln_final", "text_projection", "positional_embedding"):
                    if hasattr(m, attr):
                        try:
                            delattr(m, attr)
                        except Exception:
                            pass
                gc.collect()
                # L1: model 最后赋值 —— 并发读者要么看到全 None、要么全就绪,
                # 不会出现 model 已设而 pp/dev 仍 None 的中间态。
                _state["pp"], _state["dev"] = pp, dev
                _state["model"] = m
    return _state

@torch.no_grad()
def embed_image(img: "Image.Image"):
    s = _ensure()
    x = s["pp"](img.convert("RGB")).unsqueeze(0).to(s["dev"])
    v = F.normalize(s["model"].encode_image(x), dim=-1).squeeze(0).float().cpu().numpy()
    _state["dim"] = int(v.shape[0])
    return v

def embed_bytes(b: bytes):
    return embed_image(Image.open(io.BytesIO(b)))

def embed_path(p: str):
    return embed_image(Image.open(p))
