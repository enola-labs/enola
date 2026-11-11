class Repo:
    def save(self):
        print("save")


class Helper:
    def work(self):
        print("work")


class Base:
    def __init__(self):
        self.helper = Helper()

    def run(self):
        self.step()

    def common(self):
        self.helper.work()

    def step(self):
        raise NotImplementedError
