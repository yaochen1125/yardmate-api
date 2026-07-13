"""BioCLIP-2 嵌入(P0 选定模型)。单例懒加载。给 build_index 和 app 共用。"""
import io, warnings, threading
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
                _state.update(model=m, pp=pp, dev=dev)
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
