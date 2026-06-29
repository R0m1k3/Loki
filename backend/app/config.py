"""Configuration de l'application, chargée depuis l'environnement."""
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Réglages globaux de Loki (surchargés par variables d'environnement)."""

    ollama_host: str = "http://host.docker.internal:11434"
    default_model: str = "llama3.1:8b"
    workspace_dir: str = "/workspace"
    data_dir: str = "/data"
    port: int = 8080

    # Override manuel de la VRAM (Mo) si la détection GPU échoue dans le
    # conteneur (utile sur Unraid où le GPU est sur l'hôte / un autre conteneur).
    gpu_vram_mb: int = 0
    gpu_name: str = ""

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")


settings = Settings()
