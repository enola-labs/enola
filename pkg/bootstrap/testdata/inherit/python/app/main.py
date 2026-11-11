from app.lib import Base, Repo


class Concrete(Base):
    def __init__(self):
        super().__init__()
        self.repo = Repo()

    def step(self):
        self.repo.save()

    def handle(self):
        self.common()
        self.helper.work()


def main():
    c = Concrete()
    c.run()
    c.handle()


if __name__ == "__main__":
    main()
