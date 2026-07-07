"""Configuration de l'application, chargée depuis l'environnement."""
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Réglages globaux de Loki (surchargés par variables d'environnement)."""

    ollama_host: str = "http://host.docker.internal:11434"
    default_model: str = "gemma4:12b"
    workspace_dir: str = "/workspace"
    data_dir: str = "/data"
    port: int = 8080

    # Marqueur de build injecté à la construction de l'image (git sha court).
    # Permet de vérifier que l'image déployée est bien à jour.
    loki_version: str = "dev"

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")


settings = Settings()
