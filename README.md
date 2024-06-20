# CNI Plugin Migrator 🚀

I created this tool for work, and it's not thoroughly documented 😅. This tool migrates from Calico (installed by RKE1) to Cilium and can also roll back from Cilium to Calico. Other CNI plugins are not included.

Feel free to explore by following [this documentation](https://docs.cilium.io/en/latest/installation/k8s-install-migration/). There are also some useful scripts in the `scripts` directory.

## How to Run? 🏃

Make sure your Go version is 1.22.0 or greater, and run:

```sh
go run .
```
to migrate from Calico to Cilium, or run:

```sh
go run . -rollback
```
to revert from Cilium back to Calico.

## Releases 📦

The current version is 1.0.0, built on Linux x86_64 for Linux x86_64 only.

Happy migrating! 🛠️✨
